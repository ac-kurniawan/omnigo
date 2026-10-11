package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureOpenAIBody runs one ChatCompletion against a fake upstream and returns
// the raw request body the provider sent.
func captureOpenAIBody(t *testing.T, req ChatRequest) []byte {
	t.Helper()
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[]}`))
	}))
	defer srv.Close()

	p := NewOpenAI(Config{Name: "openai", BaseURL: srv.URL}, staticStore{Credentials{APIKey: "sk-test"}})
	rec := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if captured == nil {
		t.Fatal("upstream never received a request")
	}
	return captured
}

// A combo target may carry an effort suffix (o3-mini-high). OpenAI carries the
// effort in the body, so the suffix must be stripped to the base model and
// turned into a reasoning_effort field.
func TestOpenAIChatStripsEffortSuffixAndInjectsEffort(t *testing.T) {
	raw := captureOpenAIBody(t, ChatRequest{
		Model:  "o3-mini-high",
		Stream: false,
		Raw:    []byte(`{"model":"o3-mini-high","messages":[{"role":"user","content":"hi"}]}`),
	})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, raw)
	}
	if got := body["model"]; got != "o3-mini" {
		t.Fatalf("upstream model = %#v, want o3-mini (suffix stripped)", got)
	}
	if got := body["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", got)
	}
}

// The suffix wins over a client-supplied reasoning_effort.
func TestOpenAIChatSuffixOverridesClientEffort(t *testing.T) {
	raw := captureOpenAIBody(t, ChatRequest{
		Model:  "o3-mini-low",
		Stream: false,
		Raw:    []byte(`{"model":"o3-mini-low","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
	})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if got := body["reasoning_effort"]; got != "low" {
		t.Fatalf("reasoning_effort = %#v, want low (suffix wins)", got)
	}
}

// A client-supplied reasoning_effort survives when the target has no suffix.
func TestOpenAIChatKeepsClientEffortWithoutASuffix(t *testing.T) {
	raw := captureOpenAIBody(t, ChatRequest{
		Model:  "o3-mini",
		Stream: false,
		Raw:    []byte(`{"model":"o3-mini","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
	})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if got := body["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %#v, want high (untouched)", got)
	}
}

// A plain model with no suffix and no developer role is forwarded verbatim, so
// the rewrite path never disturbs the common case.
func TestOpenAIChatForwardsAPlainBodyByteForByte(t *testing.T) {
	raw := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`)
	got := captureOpenAIBody(t, ChatRequest{Model: "gpt-4o", Stream: false, Raw: raw, Parsed: ParseBody(raw)})
	if string(got) != string(raw) {
		t.Fatalf("upstream body = %s, want it forwarded unchanged: %s", got, raw)
	}
}
