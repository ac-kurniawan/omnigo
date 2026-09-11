package provider

import (
	"context"
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
