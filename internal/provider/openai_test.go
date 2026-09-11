package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newOpenAI(t *testing.T, upstream http.Handler) Provider {
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	store := staticStore{Credentials{APIKey: "sk-test"}}
	return NewOpenAI(Config{Name: "openai", BaseURL: srv.URL}, store)
}

func TestOpenAIChatRewritesModelInForwardedBody(t *testing.T) {
	var gotModel string
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		gotModel = body.Model
		w.Write([]byte(`{}`))
	}))
	req := ChatRequest{
		Model: "routers9/deepseek-v4-flash-0731",
		Raw:   []byte(`{"model":"myrouter/routers9/deepseek-v4-flash-0731","messages":[]}`),
	}
	if err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder()); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if gotModel != "routers9/deepseek-v4-flash-0731" {
		t.Fatalf("upstream model = %q, want bare resolved id", gotModel)
	}
}

func TestOpenAIChatUpstreamErrorReturnsError(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	rec := httptest.NewRecorder()
	req := ChatRequest{
		Model: "nonexistent",
		Raw:   []byte(`{"model":"nonexistent","messages":[]}`),
	}
	err := p.ChatCompletion(context.Background(), req, rec)
	if err == nil {
		t.Fatal("expected error on upstream non-200 status")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("recorder body = %q, expected empty", rec.Body.String())
	}
}

func TestOpenAITimeoutConfigured(t *testing.T) {
	store := staticStore{Credentials{APIKey: "sk-test"}}
	p1 := NewOpenAI(Config{Name: "openai", BaseURL: "https://example.com", Timeout: 15 * time.Second}, store)
	op1, ok := p1.(*openAIProvider)
	if !ok || op1.client.Timeout != 15*time.Second {
		t.Fatalf("client.Timeout = %v, want 15s", op1.client.Timeout)
	}

	// Default when unset or <= 0
	p2 := NewOpenAI(Config{Name: "openai", BaseURL: "https://example.com"}, store)
	op2, ok := p2.(*openAIProvider)
	if !ok || op2.client.Timeout != 30*time.Second {
		t.Fatalf("client.Timeout = %v, want default 30s", op2.client.Timeout)
	}
}

type staticStore struct{ c Credentials }

func (s staticStore) Get() Credentials      { return s.c }
func (s staticStore) Put(Credentials) error { return nil }

func TestOpenAIModels(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`))
	}))
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "gpt-4o" {
		t.Fatalf("models = %+v", models)
	}
}

func TestOpenAIChatStreamsPassthrough(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("auth = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	rec := httptest.NewRecorder()
	req := ChatRequest{Model: "gpt-4o", Stream: true, Messages: []Message{{Role: "user", Content: "hi"}}}
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestOpenAITest(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	}))
	start := time.Now()
	res := p.Test(context.Background())
	if !res.OK {
		t.Fatalf("Test = %+v", res)
	}
	if res.LatencyMS < 0 || res.LatencyMS > time.Since(start).Milliseconds()+5 {
		t.Fatalf("latency = %dms", res.LatencyMS)
	}
}
