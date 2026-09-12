package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
