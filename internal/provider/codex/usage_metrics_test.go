package codex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestCodexReportsUsageForEveryCompletedUpstream(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, event("response.output_text.delta", map[string]any{"delta": "ok"}))
		_, _ = io.WriteString(w, event("response.completed", map[string]any{"response": map[string]any{
			"status": "completed",
			"usage": map[string]any{
				"input_tokens": 10, "output_tokens": 7,
				"input_tokens_details":        map[string]any{"cached_tokens": 3},
				"output_tokens_details":       map[string]any{"reasoning_tokens": 2},
				"cache_creation_input_tokens": 1,
			},
		}}))
	}))
	defer server.Close()

	store := &poolCredStore{creds: []provider.Credentials{
		{AccessToken: "access-1", AccountID: "account-1", ExpiresAt: time.Now().Add(time.Hour)},
		{AccessToken: "access-2", AccountID: "account-2", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL, Timeout: time.Second}, store)

	var got []provider.TokenUsage
	ctx := provider.WithUsageSink(context.Background(), func(usage provider.TokenUsage) {
		got = append(got, usage)
	})
	err := p.ChatCompletion(ctx, provider.ChatRequest{
		Model: "gpt-5.3-codex", Stream: true,
		Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}, httptest.NewRecorder())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("samples = %+v, want the completed upstream only", got)
	}
	want := provider.TokenUsage{InputTokens: 10, OutputTokens: 7, CachedTokens: 3, CacheCreationInputTokens: 1, ReasoningTokens: 2, Account: "account-2", Present: true}
	if got[0] != want {
		t.Fatalf("usage = %+v, want %+v", got[0], want)
	}
}

func TestCodexDropsUsageWhenResponseOmitsIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, event("response.completed", map[string]any{"response": map[string]any{"status": "completed"}}))
	}))
	defer server.Close()
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL, Timeout: time.Second}, store)

	var got int
	ctx := provider.WithUsageSink(context.Background(), func(provider.TokenUsage) { got++ })
	if err := p.ChatCompletion(ctx, provider.ChatRequest{Model: "gpt", Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, httptest.NewRecorder()); err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("samples = %d, want none when usage is absent", got)
	}
}

func TestCodexReportsUsageOnIncompleteResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, event("response.incomplete", map[string]any{"response": map[string]any{"status": "incomplete", "usage": map[string]any{"input_tokens": 2, "output_tokens": 3}}}))
	}))
	defer server.Close()
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL, Timeout: time.Second}, store)

	var got []provider.TokenUsage
	ctx := provider.WithUsageSink(context.Background(), func(usage provider.TokenUsage) { got = append(got, usage) })
	if err := p.ChatCompletion(ctx, provider.ChatRequest{Model: "gpt", Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, httptest.NewRecorder()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].InputTokens != 2 || got[0].OutputTokens != 3 || !got[0].Present {
		t.Fatalf("usage = %+v", got)
	}
}
