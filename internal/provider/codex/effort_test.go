package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// captureBody runs one ChatCompletion against a fake upstream and returns the
// decoded request body the provider sent.
func captureBody(t *testing.T, model, raw string) map[string]any {
	t.Helper()
	return captureBodyFor(t, provider.ChatRequest{Model: model, Stream: true, Raw: []byte(raw)})
}

func captureBodyFor(t *testing.T, req provider.ChatRequest) map[string]any {
	t.Helper()
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, codexTestStream())
	}))
	defer server.Close()

	store := &memoryCredStore{creds: provider.Credentials{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		AccountID:    "account-1",
		ExpiresAt:    time.Now().Add(time.Hour),
	}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL + "/responses", Timeout: time.Second}, store)
	rr := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), req, rr); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if captured == nil {
		t.Fatal("upstream never received a request")
	}
	return captured
}

// A target's effort is carried in the body's reasoning object, next to the bare
// model slug.
func TestChatCompletionInjectsTargetEffort(t *testing.T) {
	body := captureBodyFor(t, provider.ChatRequest{
		Model:  "gpt-5.5",
		Effort: "high",
		Stream: true,
		Raw:    []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	})

	if got := body["model"]; got != "gpt-5.5" {
		t.Fatalf("upstream model = %#v, want gpt-5.5", got)
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %#v, want an object carrying the effort", body["reasoning"])
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning.effort = %#v, want high", reasoning["effort"])
	}
}

// The target's effort wins over a client-supplied reasoning_effort.
func TestChatCompletionTargetEffortOverridesClientEffort(t *testing.T) {
	body := captureBodyFor(t, provider.ChatRequest{
		Model:  "gpt-5.5",
		Effort: "low",
		Stream: true,
		Raw:    []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
	})

	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" {
		t.Fatalf("reasoning.effort = %#v, want low (target wins)", reasoning["effort"])
	}
}

// The effort rides on the base model's profile: gpt-6-astra is a Lite model, so
// the profile marker (reasoning.context) proves the lookup ran on the base.
func TestChatCompletionResolvesProfileForAnEffortTarget(t *testing.T) {
	body := captureBodyFor(t, provider.ChatRequest{
		Model:  "gpt-6-astra",
		Effort: "ultra",
		Stream: true,
		Raw:    []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	})

	if got := body["model"]; got != "gpt-6-astra" {
		t.Fatalf("upstream model = %#v, want gpt-6-astra", got)
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "ultra" {
		t.Fatalf("reasoning.effort = %#v, want ultra", reasoning["effort"])
	}
	if reasoning["context"] != "all_turns" {
		t.Fatalf("reasoning.context = %#v, want all_turns (Lite profile applied)", reasoning["context"])
	}
}

// A plain model with no effort is forwarded unchanged.
func TestChatCompletionLeavesAPlainModelAlone(t *testing.T) {
	body := captureBody(t, "gpt-5.5", `{"messages":[{"role":"user","content":"hi"}]}`)
	if got := body["model"]; got != "gpt-5.5" {
		t.Fatalf("upstream model = %#v, want gpt-5.5", got)
	}
}

// A model id that merely ends in a level token (gpt-oss-120b-medium is a real
// Ollama-style id) is not split any more: the id is the model.
func TestChatCompletionLeavesASuffixLookingIdAlone(t *testing.T) {
	body := captureBody(t, "gpt-oss-120b-medium", `{"messages":[{"role":"user","content":"hi"}]}`)
	if got := body["model"]; got != "gpt-oss-120b-medium" {
		t.Fatalf("upstream model = %#v, want gpt-oss-120b-medium", got)
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, exists := reasoning["effort"]; exists && effort != "" {
			t.Fatalf("reasoning.effort = %#v, want none for a non-effort family", effort)
		}
	}
}

// An effort on a model with no reasoning profile is rejected instead of being
// silently dropped: the operator picked a level the model cannot take.
func TestChatCompletionRejectsEffortOnANonReasoningModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a model that takes no effort")
	}))
	defer server.Close()

	store := &memoryCredStore{creds: provider.Credentials{
		AccessToken: "access-token",
		AccountID:   "account-1",
		ExpiresAt:   time.Now().Add(time.Hour),
	}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL + "/responses", Timeout: time.Second}, store)
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{
		Model:  "gpt-oss-120b",
		Effort: "high",
		Stream: true,
		Raw:    []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}, httptest.NewRecorder())
	if err == nil {
		t.Fatal("expected an error for an effort on a model with no reasoning profile")
	}
}
