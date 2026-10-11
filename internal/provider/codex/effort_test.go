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
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{
		Model:  model,
		Stream: true,
		Raw:    []byte(raw),
	}, rr); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if captured == nil {
		t.Fatal("upstream never received a request")
	}
	return captured
}

// A combo target may carry an effort suffix (gpt-5.5-high). Codex sends the
// model as a literal slug, so the suffix must be stripped to the base model and
// turned into a body-level reasoning effort.
func TestChatCompletionStripsEffortSuffixAndInjectsEffort(t *testing.T) {
	body := captureBody(t, "gpt-5.5-high", `{"messages":[{"role":"user","content":"hi"}]}`)

	if got := body["model"]; got != "gpt-5.5" {
		t.Fatalf("upstream model = %#v, want gpt-5.5 (suffix stripped)", got)
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %#v, want an object carrying the effort", body["reasoning"])
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning.effort = %#v, want high", reasoning["effort"])
	}
}

// The suffix wins over a client-supplied reasoning_effort.
func TestChatCompletionSuffixOverridesClientEffort(t *testing.T) {
	body := captureBody(t, "gpt-5.5-low", `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)

	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" {
		t.Fatalf("reasoning.effort = %#v, want low (suffix wins)", reasoning["effort"])
	}
}

// A suffixed id must still resolve its base model's profile: gpt-6-astra is a
// Lite model, so the profile marker (reasoning.context) proves the lookup ran
// on the stripped base rather than missing on the suffixed id.
func TestChatCompletionResolvesProfileForASuffixedModel(t *testing.T) {
	body := captureBody(t, "gpt-6-astra-ultra", `{"messages":[{"role":"user","content":"hi"}]}`)

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

// A plain model with no suffix is forwarded unchanged.
func TestChatCompletionLeavesAPlainModelAlone(t *testing.T) {
	body := captureBody(t, "gpt-5.5", `{"messages":[{"role":"user","content":"hi"}]}`)
	if got := body["model"]; got != "gpt-5.5" {
		t.Fatalf("upstream model = %#v, want gpt-5.5", got)
	}
}
