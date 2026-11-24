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

// A target's effort is carried in the body next to the bare model id, which is
// what OpenAI expects.
func TestOpenAIChatInjectsTargetEffort(t *testing.T) {
	raw := captureOpenAIBody(t, ChatRequest{
		Model:  "o3-mini",
		Effort: "high",
		Raw:    []byte(`{"model":"o3-mini","messages":[{"role":"user","content":"hi"}]}`),
	})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, raw)
	}
	if got := body["model"]; got != "o3-mini" {
		t.Fatalf("upstream model = %#v, want o3-mini", got)
	}
	if got := body["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", got)
	}
}

// The target's effort wins over a client-supplied reasoning_effort.
func TestOpenAIChatTargetEffortOverridesClientEffort(t *testing.T) {
	raw := captureOpenAIBody(t, ChatRequest{
		Model:  "o3-mini",
		Effort: "low",
		Raw:    []byte(`{"model":"o3-mini","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
	})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if got := body["reasoning_effort"]; got != "low" {
		t.Fatalf("reasoning_effort = %#v, want low (target wins)", got)
	}
}

// A client-supplied reasoning_effort survives when the target has none.
func TestOpenAIChatKeepsClientEffortWithoutATargetEffort(t *testing.T) {
	raw := captureOpenAIBody(t, ChatRequest{
		Model: "o3-mini",
		Raw:   []byte(`{"model":"o3-mini","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
	})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if got := body["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %#v, want high (untouched)", got)
	}
}

// A plain model with no effort and no developer role is forwarded verbatim, so
// the rewrite path never disturbs the common case.
func TestOpenAIChatForwardsAPlainBodyByteForByte(t *testing.T) {
	raw := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`)
	got := captureOpenAIBody(t, ChatRequest{Model: "gpt-4o", Raw: raw, Parsed: ParseBody(raw)})
	if string(got) != string(raw) {
		t.Fatalf("upstream body = %s, want it forwarded unchanged: %s", got, raw)
	}
}

// A model id that merely ends in a level token is a real upstream id: the
// provider must not split it into a base plus an effort any more.
func TestOpenAIChatForwardsASuffixLookingIdUnchanged(t *testing.T) {
	raw := []byte(`{"model":"gpt-oss-120b-medium","messages":[{"role":"user","content":"hi"}]}`)
	got := captureOpenAIBody(t, ChatRequest{Model: "gpt-oss-120b-medium", Raw: raw, Parsed: ParseBody(raw)})
	if string(got) != string(raw) {
		t.Fatalf("upstream body = %s, want the literal id forwarded unchanged: %s", got, raw)
	}
}

// The effort rewrite must keep the developer->system normalisation the plain
// Body() path performs; otherwise the same logical request forwards a role the
// upstream does not recognise and the request hangs.
func TestOpenAIChatNormalisesDeveloperRoleOnTheEffortPath(t *testing.T) {
	raw := captureOpenAIBody(t, ChatRequest{
		Model:  "o3-mini",
		Effort: "high",
		Raw:    []byte(`{"model":"o3-mini","messages":[{"role":"developer","content":"be nice"}]}`),
	})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("messages = %#v, want one message", body["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if got := first["role"]; got != "system" {
		t.Fatalf("role = %#v, want system (developer must be normalised on the effort path too)", got)
	}
	if got := body["model"]; got != "o3-mini" {
		t.Fatalf("model = %#v, want o3-mini", got)
	}
	if got := body["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", got)
	}
}
