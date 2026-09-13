package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// fakeProvider implements provider.Provider for routing tests.
type fakeProvider struct {
	name   string
	models []provider.Model
	chat   func(provider.ChatRequest) error
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Models(ctx context.Context) ([]provider.Model, error) {
	return f.models, nil
}
func (f *fakeProvider) Test(ctx context.Context) provider.TestResult {
	return provider.TestResult{OK: true}
}
func (f *fakeProvider) ChatCompletion(ctx context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
	if f.chat != nil {
		return f.chat(req)
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	return nil
}

func TestChatRoutesToCombo(t *testing.T) {
	var gotModel string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, chat: func(r provider.ChatRequest) error {
			gotModel = r.Model
			return nil
		}}
	})
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{"gpt-4o"}}},
		Combos:    []config.Combo{{Name: "auto", Strategy: "priority", Targets: []config.ComboTarget{{Provider: "openai", Model: "gpt-4o"}}}},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{
		ProviderSecrets: map[string]vault.ProviderSecret{"openai": {APIKey: "sk-x"}},
		ClientKeys:      []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}},
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	if gotModel != "gpt-4o" {
		t.Fatalf("model = %q", gotModel)
	}
}

func TestChatAcceptsMultimodalContent(t *testing.T) {
	var got provider.ChatRequest
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, chat: func(r provider.ChatRequest) error {
			got = r
			return nil
		}}
	})
	cfg := &config.Config{Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{"gpt-4o"}}}}
	rawKey, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rr := httptest.NewRecorder()

	testRouter(t, cfg, v).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	parts, ok := got.Messages[0].Content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v", got.Messages[0].Content)
	}
	if string(got.Raw) != body {
		t.Fatalf("raw request changed: %s", got.Raw)
	}
	forwarded, err := got.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if !strings.Contains(string(forwarded), `"image_url"`) || !strings.Contains(string(forwarded), "data:image/png;base64,abc") {
		t.Fatalf("forwarded payload lost image part: %s", forwarded)
	}
}

func TestChatUnknownModelNotFound(t *testing.T) {
	cfg := &config.Config{}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"nope","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestChatRejectsBodyLargerThan10MB(t *testing.T) {
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(strings.Repeat(" ", (10<<20)+1)))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()

	testRouter(t, &config.Config{}, v).ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rr.Code, rr.Body.String())
	}
}

func TestProviderReceivesConfiguredTimeout(t *testing.T) {
	var gotTimeout time.Duration
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		gotTimeout = cfg.Timeout
		return &fakeProvider{name: cfg.Name}
	})
	cfg := &config.Config{
		Server: config.Server{Timeout: "40s"},
		Providers: []config.Provider{
			{Name: "openai-custom", Type: "openai", Timeout: "12s"},
			{Name: "openai-default", Type: "openai"},
		},
	}
	v := &vault.Vault{}
	_, _ = buildProvider(cfg.Providers[0], vault.NewMemoryStore(v), cfg.DefaultTimeout())
	if gotTimeout != 12*time.Second {
		t.Fatalf("custom provider timeout = %v, want 12s", gotTimeout)
	}
	_, _ = buildProvider(cfg.Providers[1], vault.NewMemoryStore(v), cfg.DefaultTimeout())
	if gotTimeout != 40*time.Second {
		t.Fatalf("default provider timeout = %v, want 40s (from server)", gotTimeout)
	}
}

func TestChatDisabledModelRejected(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name}
	})
	cfg := &config.Config{
		Providers: []config.Provider{
			{
				Name:           "openai",
				Type:           "openai",
				BaseURL:        "https://x",
				Models:         []string{"gpt-4o"},
				DisabledModels: []string{"gpt-3.5-turbo"},
			},
		},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}

	// Request openai/gpt-3.5-turbo directly
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"openai/gpt-3.5-turbo","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for disabled model", rr.Code)
	}
}

func TestComboFallsBackWhenFirstModelDisabled(t *testing.T) {
	var gotModel string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, chat: func(r provider.ChatRequest) error {
			gotModel = r.Model
			return nil
		}}
	})
	cfg := &config.Config{
		Providers: []config.Provider{
			{
				Name:           "openai",
				Type:           "openai",
				BaseURL:        "https://x",
				Models:         []string{"gpt-4o"},
				DisabledModels: []string{"gpt-3.5-turbo"},
			},
		},
		Combos: []config.Combo{
			{
				Name:     "auto",
				Strategy: "priority",
				Targets: []config.ComboTarget{
					{Provider: "openai", Model: "gpt-3.5-turbo"}, // disabled!
					{Provider: "openai", Model: "gpt-4o"},        // active!
				},
			},
		},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotModel != "gpt-4o" {
		t.Fatalf("model = %q, want gpt-4o (first target was disabled)", gotModel)
	}
}

func TestChatDisabledProviderRejected(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name}
	})
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{"gpt-4o"}, Disabled: true},
		},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"openai/gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for disabled provider", rr.Code)
	}
}

func TestComboFallsBackWhenProviderDisabled(t *testing.T) {
	var gotModel string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, chat: func(r provider.ChatRequest) error {
			gotModel = r.Model
			return nil
		}}
	})
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "groq", Type: "openai", BaseURL: "https://x", Models: []string{"llama-3"}, Disabled: true},
			{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{"gpt-4o"}},
		},
		Combos: []config.Combo{
			{
				Name:     "auto",
				Strategy: "priority",
				Targets: []config.ComboTarget{
					{Provider: "groq", Model: "llama-3"},
					{Provider: "openai", Model: "gpt-4o"},
				},
			},
		},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotModel != "gpt-4o" {
		t.Fatalf("model = %q, want gpt-4o (first provider was disabled)", gotModel)
	}
}

func TestChatDirectModelWithMultipleSlashes(t *testing.T) {
	var gotModel string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, chat: func(r provider.ChatRequest) error {
			gotModel = r.Model
			return nil
		}}
	})
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "myrouter", Type: "openai", BaseURL: "https://x", Models: []string{"routers9/deepseek-v4-flash-0731"}},
		},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{
		ProviderSecrets: map[string]vault.ProviderSecret{"myrouter": {APIKey: "sk-x"}},
		ClientKeys:      []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}},
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"myrouter/routers9/deepseek-v4-flash-0731","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	if gotModel != "routers9/deepseek-v4-flash-0731" {
		t.Fatalf("model = %q, want routers9/deepseek-v4-flash-0731", gotModel)
	}
}

func TestChatFillFirstUsesTrackerAndDrains(t *testing.T) {
	var calls []string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{
			name: cfg.Name,
			chat: func(r provider.ChatRequest) error {
				calls = append(calls, cfg.Name)
				if cfg.Name == "prov-a" {
					return fmt.Errorf("rate limited (429)")
				}
				return nil
			},
		}
	})

	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "prov-a", Type: "openai", BaseURL: "https://a", Models: []string{"m1"}},
			{Name: "prov-b", Type: "openai", BaseURL: "https://b", Models: []string{"m2"}},
		},
		Combos: []config.Combo{
			{
				Name:     "smart",
				Strategy: "fill-first",
				DrainTTL: "1m",
				Targets: []config.ComboTarget{
					{Provider: "prov-a", Model: "m1"},
					{Provider: "prov-b", Model: "m2"},
				},
			},
		},
	}

	tr := combo.NewTracker("")
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}

	router := NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, tr, "test-version")

	// Request 1: prov-a fails, marks drained, falls back to prov-b
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"hi"}]}`))
	req1.Header.Set("Authorization", "Bearer "+raw)
	rr1 := httptest.NewRecorder()
	router.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("req1 status = %d: %s", rr1.Code, rr1.Body.String())
	}
	if len(calls) != 2 || calls[0] != "prov-a" || calls[1] != "prov-b" {
		t.Fatalf("calls = %v, want [prov-a prov-b]", calls)
	}

	if !tr.IsDrained(combo.Target{Provider: "prov-a", Model: "m1"}) {
		t.Fatal("prov-a/m1 should be marked drained")
	}

	// Request 2: prov-a is already drained -> fill-first directly calls prov-b!
	calls = nil
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"hi again"}]}`))
	req2.Header.Set("Authorization", "Bearer "+raw)
	rr2 := httptest.NewRecorder()
	router.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("req2 status = %d: %s", rr2.Code, rr2.Body.String())
	}
	if len(calls) != 1 || calls[0] != "prov-b" {
		t.Fatalf("req2 calls = %v, want only [prov-b] (skipped drained prov-a)", calls)
	}
}

func TestComboFallsBackWhenUpstreamErrors(t *testing.T) {
	var gotModel string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{
			name: cfg.Name,
			chat: func(r provider.ChatRequest) error {
				if cfg.Name == "bad" {
					return fmt.Errorf("upstream status 500")
				}
				gotModel = r.Model
				return nil
			},
		}
	})
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "bad", Type: "openai", BaseURL: "https://bad", Models: []string{"m-bad"}},
			{Name: "good", Type: "openai", BaseURL: "https://good", Models: []string{"m-good"}},
		},
		Combos: []config.Combo{
			{
				Name:     "auto",
				Strategy: "priority",
				Targets: []config.ComboTarget{
					{Provider: "bad", Model: "m-bad"},
					{Provider: "good", Model: "m-good"},
				},
			},
		},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotModel != "m-good" {
		t.Fatalf("model = %q, want m-good (first target had upstream error)", gotModel)
	}
}

func TestReliableBuffersFailuresBeforeCommitting(t *testing.T) {
	var calls []string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(_ context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
			calls = append(calls, cfg.Name)
			w.Header().Set("X-Upstream", cfg.Name)
			if cfg.Name == "bad" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"secret":"upstream error"}`))
				return nil
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"provider":"good"}`))
			return nil
		}}
	})
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "bad", Type: "openai", Models: []string{"m1"}},
			{Name: "good", Type: "openai", Models: []string{"m2"}},
		},
		Combos: []config.Combo{{
			Name: "safe", Strategy: "reliable", DrainTTL: "1m",
			Targets: []config.ComboTarget{{Provider: "bad", Model: "m1"}, {Provider: "good", Model: "m2"}},
		}},
	}
	tr := combo.NewTracker("")
	rr := performChat(t, cfg, tr, `{"model":"safe","messages":[]}`)

	if rr.Code != http.StatusOK || rr.Body.String() != `{"provider":"good"}` {
		t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Upstream"); got != "good" {
		t.Fatalf("X-Upstream = %q", got)
	}
	if strings.Contains(rr.Body.String(), "upstream error") || len(calls) != 2 {
		t.Fatalf("body = %q, calls = %v", rr.Body.String(), calls)
	}
	if !tr.IsDrained(combo.Target{Provider: "bad", Model: "m1"}) {
		t.Fatal("failed target should be drained")
	}
}

func TestReliableIgnoresDuplicateWriteHeaderFromFailedAttempt(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(_ context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
			if cfg.Name == "bad" {
				w.WriteHeader(http.StatusBadGateway)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"error":"bad"}`))
				return nil
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"provider":"good"}`))
			return nil
		}}
	})
	rr := performChat(t, reliableTestConfig(), combo.NewTracker(""), `{"model":"safe","messages":[]}`)
	if rr.Code != http.StatusOK || rr.Body.String() != `{"provider":"good"}` {
		t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
	}
}

func TestReliableFallsBackOnEveryFailureClass(t *testing.T) {
	tests := []struct {
		name string
		fail func(http.ResponseWriter) error
	}{
		{"status 400", statusFailure(http.StatusBadRequest)},
		{"status 401", statusFailure(http.StatusUnauthorized)},
		{"status 403", statusFailure(http.StatusForbidden)},
		{"status 429", statusFailure(http.StatusTooManyRequests)},
		{"status 500", statusFailure(http.StatusInternalServerError)},
		{"status 502", statusFailure(http.StatusBadGateway)},
		{"status 503", statusFailure(http.StatusServiceUnavailable)},
		{"timeout", func(http.ResponseWriter) error { return context.DeadlineExceeded }},
		{"connection", func(http.ResponseWriter) error { return errors.New("dial tcp: connection refused") }},
		{"malformed", func(w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[`))
			return nil
		}},
		{"pre-commit stream", func(w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "text/event-stream")
			return io.ErrUnexpectedEOF
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
				return &responseProvider{name: cfg.Name, chat: func(_ context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
					if cfg.Name == "bad" {
						return tt.fail(w)
					}
					if req.Stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
						return nil
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
					return nil
				}}
			})
			stream := strings.Contains(tt.name, "stream")
			cfg := reliableTestConfig()
			body := fmt.Sprintf(`{"model":"safe","stream":%t,"messages":[]}`, stream)
			rr := performChat(t, cfg, combo.NewTracker(""), body)
			if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "ok") {
				t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestReliableStreamingSuccessDoesNotWriteFallbackError(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(_ context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return nil
		}}
	})
	cfg := reliableTestConfig()
	tr := combo.NewTracker("")
	rr := performChat(t, cfg, tr, `{"model":"safe","stream":true,"messages":[]}`)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), `"error"`) || !strings.Contains(rr.Body.String(), "[DONE]") {
		t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
	}
	if tr.IsDrained(combo.Target{Provider: "bad", Model: "m1"}) {
		t.Fatal("successful stream should not drain target")
	}
}

func TestReliablePostCommitStreamFailureDoesNotMixTargets(t *testing.T) {
	var calls []string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(_ context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
			calls = append(calls, cfg.Name)
			if cfg.Name == "first" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"model\":\"first\"}\n\n"))
				w.(http.Flusher).Flush()
				return io.ErrUnexpectedEOF
			}
			_, _ = w.Write([]byte("data: {\"model\":\"second\"}\n\n"))
			return nil
		}}
	})
	cfg := reliableTestConfig()
	cfg.Providers[0].Name = "first"
	cfg.Providers[1].Name = "second"
	cfg.Combos[0].Targets[0].Provider = "first"
	cfg.Combos[0].Targets[1].Provider = "second"
	tr := combo.NewTracker("")
	rr := performChat(t, cfg, tr, `{"model":"safe","stream":true,"messages":[]}`)

	if !strings.Contains(rr.Body.String(), `"first"`) || strings.Contains(rr.Body.String(), `"second"`) {
		t.Fatalf("body = %q", rr.Body.String())
	}
	if len(calls) != 1 || calls[0] != "first" {
		t.Fatalf("calls = %v, want [first]", calls)
	}
	if !tr.IsDrained(combo.Target{Provider: "first", Model: "m1"}) {
		t.Fatal("disconnected target should be drained")
	}
}

func TestRoundRobinRoutesAcrossHealthyTargets(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(_ context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
			mu.Lock()
			calls[cfg.Name]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"provider":%q}`, cfg.Name)
			return nil
		}}
	})
	cfg := reliableTestConfig()
	cfg.Combos[0].Name = "balanced-api"
	cfg.Combos[0].Strategy = "round-robin"
	tr := combo.NewTracker("")
	for range 20 {
		rr := performChat(t, cfg, tr, `{"model":"balanced-api","messages":[]}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
		}
	}
	if calls["bad"] != 10 || calls["good"] != 10 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestReliableAllTargetsFailReturnsSanitizedAggregate(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(_ context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
			return fmt.Errorf("dial failed using token sk-example-redaction-fixture")
		}}
	})
	cfg := reliableTestConfig()
	tr := combo.NewTracker("")
	rr := performChat(t, cfg, tr, `{"model":"safe","messages":[]}`)
	body := rr.Body.String()
	if rr.Code != http.StatusBadGateway || !strings.Contains(body, "bad/m1") || !strings.Contains(body, "good/m2") {
		t.Fatalf("status = %d body = %q", rr.Code, body)
	}
	if strings.Contains(body, "sk-example-redaction-fixture") {
		t.Fatalf("aggregate leaked credential: %q", body)
	}
	for _, target := range []combo.Target{{Provider: "bad", Model: "m1"}, {Provider: "good", Model: "m2"}} {
		if reason := tr.DrainReason(target); strings.Contains(reason, "sk-example-redaction-fixture") {
			t.Fatalf("drain reason leaked credential: %q", reason)
		}
	}
}

type responseProvider struct {
	name string
	chat func(context.Context, provider.ChatRequest, http.ResponseWriter) error
}

func (p *responseProvider) Name() string                                     { return p.name }
func (p *responseProvider) Models(context.Context) ([]provider.Model, error) { return nil, nil }
func (p *responseProvider) Test(context.Context) provider.TestResult {
	return provider.TestResult{OK: true}
}
func (p *responseProvider) ChatCompletion(ctx context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
	return p.chat(ctx, req, w)
}

func statusFailure(status int) func(http.ResponseWriter) error {
	return func(w http.ResponseWriter) error {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"failed"}`))
		return nil
	}
}

func reliableTestConfig() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{Name: "bad", Type: "openai", Models: []string{"m1"}},
			{Name: "good", Type: "openai", Models: []string{"m2"}},
		},
		Combos: []config.Combo{{
			Name: "safe", Strategy: "reliable", DrainTTL: "1m",
			Targets: []config.ComboTarget{{Provider: "bad", Model: "m1"}, {Provider: "good", Model: "m2"}},
		}},
	}
}

func performChat(t *testing.T, cfg *config.Config, tr *combo.Tracker, body string) *httptest.ResponseRecorder {
	t.Helper()
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, tr, "test-version").ServeHTTP(rr, req)
	return rr
}

func TestChatComboWithModelWithSlash(t *testing.T) {
	var gotModel string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, chat: func(r provider.ChatRequest) error {
			gotModel = r.Model
			return nil
		}}
	})
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "myrouter", Type: "openai", BaseURL: "https://x", Models: []string{"routers9/deepseek-v4-flash-0731"}},
		},
		Combos: []config.Combo{
			{
				Name:     "auto",
				Strategy: "priority",
				Targets: []config.ComboTarget{
					{Provider: "myrouter", Model: "routers9/deepseek-v4-flash-0731"},
				},
			},
		},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{
		ProviderSecrets: map[string]vault.ProviderSecret{"myrouter": {APIKey: "sk-x"}},
		ClientKeys:      []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}},
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	if gotModel != "routers9/deepseek-v4-flash-0731" {
		t.Fatalf("model = %q, want routers9/deepseek-v4-flash-0731", gotModel)
	}
}
