package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	// chatCtx is set when a test needs the request context, which is where
	// token usage is reported. chat is left untouched for the existing tests.
	chatCtx func(context.Context, provider.ChatRequest) error
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Models(ctx context.Context) ([]provider.Model, error) {
	return f.models, nil
}
func (f *fakeProvider) Test(ctx context.Context) provider.TestResult {
	return provider.TestResult{OK: true}
}
func (f *fakeProvider) ChatCompletion(ctx context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
	if f.chatCtx != nil {
		if err := f.chatCtx(ctx, req); err != nil {
			return err
		}
	} else if f.chat != nil {
		if err := f.chat(req); err != nil {
			return err
		}
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

func TestComboOpenAITargetsMarshalOnceAndReuseParsedBody(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, chat: func(r provider.ChatRequest) error {
			body, err := (&r).Body()
			if err != nil {
				return err
			}
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
			if cfg.Name != "third" {
				return provider.NewHTTPStatusError(http.StatusTooManyRequests, "rate limited")
			}
			return nil
		}}
	})
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "first", Type: "openai", BaseURL: "https://first.example"},
			{Name: "second", Type: "openai", BaseURL: "https://second.example"},
			{Name: "third", Type: "openai", BaseURL: "https://third.example"},
		},
		Combos: []config.Combo{{
			Name:     "auto",
			Strategy: "priority",
			Targets: []config.ComboTarget{
				{Provider: "first", Model: "gpt-a"},
				{Provider: "second", Model: "gpt-b"},
				{Provider: "third", Model: "gpt-b"},
			},
		}},
	}
	rawKey, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	body := `{"model":"auto","messages":[{"role":"developer","content":"instructions"},{"role":"user","content":"hi"}],"temperature":0.2}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rr := httptest.NewRecorder()

	testRouter(t, cfg, v).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(bodies) != 3 {
		t.Fatalf("targets encoded = %d, want 3", len(bodies))
	}
	if &bodies[1][0] != &bodies[2][0] {
		t.Fatal("identical OpenAI targets did not share encoded bytes")
	}
	for i, encoded := range bodies {
		var got struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
			Temperature float64 `json:"temperature"`
		}
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("target %d body: %v", i, err)
		}
		wantModel := "gpt-a"
		if i > 0 {
			wantModel = "gpt-b"
		}
		if got.Model != wantModel || got.Temperature != 0.2 || len(got.Messages) != 2 || got.Messages[0].Role != "system" {
			t.Fatalf("target %d body = %+v", i, got)
		}
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

func TestRouterCachesProviderInstances(t *testing.T) {
	var builds atomic.Int32
	provider.Register("cached", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		builds.Add(1)
		return &fakeProvider{name: cfg.Name}
	})
	cfg := &config.Config{Providers: []config.Provider{{Name: "cached", Type: "cached", Models: []string{"model"}}}}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	router := testRouter(t, cfg, v)

	for range 2 {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"cached/model","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+raw)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
		}
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("provider builds = %d, want 1", got)
	}
}

func TestProviderReceivesConfiguredTransport(t *testing.T) {
	var got http.RoundTripper
	provider.Register("transport-test", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		got = cfg.Transport
		return &fakeProvider{name: cfg.Name}
	})
	registry := newProviderRegistry(vault.NewMemoryStore(&vault.Vault{}))
	cfg := &config.Config{Providers: []config.Provider{{Name: "transport", Type: "transport-test"}}}
	registry.ensure(cfg)
	if got != registry.transport {
		t.Fatal("provider did not receive shared transport")
	}
}

func TestProviderRegistryReloadsChangedProviders(t *testing.T) {
	var builds atomic.Int32
	provider.Register("reload-test", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		builds.Add(1)
		return &fakeProvider{name: cfg.Name}
	})
	registry := newProviderRegistry(vault.NewMemoryStore(&vault.Vault{}))
	cfg := &config.Config{Providers: []config.Provider{{Name: "provider", Type: "reload-test", BaseURL: "https://one"}}}
	registry.ensure(cfg)
	first, ok := registry.Get(cfg, "provider")
	if !ok {
		t.Fatal("provider missing after initial load")
	}

	cfg2 := &config.Config{Providers: []config.Provider{{Name: "provider", Type: "reload-test", BaseURL: "https://two"}}}
	registry.ensure(cfg2)
	second, ok := registry.Get(cfg2, "provider")
	if !ok {
		t.Fatal("provider missing after reload")
	}
	if first == second || builds.Load() != 2 {
		t.Fatalf("reload did not rebuild provider: builds = %d", builds.Load())
	}
}

func BenchmarkResolveDirectCachedProvider(b *testing.B) {
	provider.Register("benchmark", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name}
	})
	cfg := &config.Config{Providers: []config.Provider{{Name: "benchmark", Type: "benchmark", Models: []string{"model"}}}}
	registry := newProviderRegistry(vault.NewMemoryStore(&vault.Vault{}))
	registry.ensure(cfg)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, _, ok := resolveDirect(cfg, registry, "benchmark/model"); !ok {
			b.Fatal("provider not resolved")
		}
	}
}

func TestProviderReceivesConfiguredTimeouts(t *testing.T) {
	var got provider.Config
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		got = cfg
		return &fakeProvider{name: cfg.Name}
	})
	cfg := &config.Config{
		Server: config.Server{Timeout: "40s", StreamTimeout: "15m"},
		Providers: []config.Provider{
			{Name: "openai-custom", Type: "openai", Timeout: "12s", StreamTimeout: "1m"},
			{Name: "openai-default", Type: "openai"},
		},
	}
	v := &vault.Vault{}
	_, _ = buildProvider(cfg.Providers[0], vault.NewMemoryStore(v), nil, cfg.Timeouts())
	if got.Timeout != 12*time.Second {
		t.Fatalf("custom provider timeout = %v, want 12s", got.Timeout)
	}
	if got.StreamTimeout != time.Minute {
		t.Fatalf("custom provider stream timeout = %v, want 1m", got.StreamTimeout)
	}
	_, _ = buildProvider(cfg.Providers[1], vault.NewMemoryStore(v), nil, cfg.Timeouts())
	if got.Timeout != 40*time.Second {
		t.Fatalf("default provider timeout = %v, want 40s (from server)", got.Timeout)
	}
	if got.StreamTimeout != 15*time.Minute {
		t.Fatalf("default provider stream timeout = %v, want 15m (from server)", got.StreamTimeout)
	}
}

// A streaming generation whose response headers arrive within the configured
// provider timeout must succeed; the transport must not impose an independent
// cap that aborts it early.
func TestChatStreamAllowsConfiguredTimeToFirstByte(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Providers: []config.Provider{{
			Name:    "openai",
			Type:    "openai",
			BaseURL: upstream.URL,
			Models:  []string{"gpt-4o"},
			Timeout: "300ms",
		}},
	}
	rawKey, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{
		ProviderSecrets: map[string]vault.ProviderSecret{"openai": {APIKey: "sk-x"}},
		ClientKeys:      []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"openai/gpt-4o","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200 (headers arrived within 300ms timeout)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "ok") {
		t.Fatalf("stream body missing data: %s", rr.Body.String())
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

func TestChatDirectModelMustBeListed(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name}
	})
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}}},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"openai/gpt-4o-expensive","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a model not in the provider catalog", rr.Code)
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

	router := NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, tr, "test-version", nil)

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

// A combo budget caps the whole chain, so a chain of targets that each stall
// returns a timeout rather than running on until the client gives up, and the
// client is told to retry.
func TestComboTimeoutReturnsGatewayTimeout(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(ctx context.Context, _ provider.ChatRequest, _ http.ResponseWriter) error {
			<-ctx.Done()
			return ctx.Err()
		}}
	})
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "a", Type: "openai", Models: []string{"m1"}},
			{Name: "b", Type: "openai", Models: []string{"m2"}},
			{Name: "c", Type: "openai", Models: []string{"m3"}},
		},
		Combos: []config.Combo{{
			Name: "safe", Strategy: "reliable", Timeout: "300ms",
			Targets: []config.ComboTarget{{Provider: "a", Model: "m1"}, {Provider: "b", Model: "m2"}, {Provider: "c", Model: "m3"}},
		}},
	}

	start := time.Now()
	rr := performChat(t, cfg, nil, `{"model":"safe","messages":[]}`)
	elapsed := time.Since(start)

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After on combo timeout")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("chain ran %s, want it bounded near 300ms", elapsed)
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
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"))
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
	NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, tr, "test-version", nil).ServeHTTP(rr, req)
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

func TestProviderConcurrencyLimitInCombo(t *testing.T) {
	inFlight := make(chan struct{})
	done := make(chan struct{})
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(_ context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
			if cfg.Name == "slow" {
				inFlight <- struct{}{}
				<-done
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"provider":"slow"}`))
				return nil
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"provider":"fast"}`))
			return nil
		}}
	})

	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "slow", Type: "openai", BaseURL: "https://slow", Models: []string{"m1"}, MaxConcurrency: 1},
			{Name: "fast", Type: "openai", BaseURL: "https://fast", Models: []string{"m2"}},
		},
		Combos: []config.Combo{
			{
				Name:     "auto",
				Strategy: "priority",
				Targets: []config.ComboTarget{
					{Provider: "slow", Model: "m1"},
					{Provider: "fast", Model: "m2"},
				},
			},
		},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{
		ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}},
	}
	router := testRouter(t, cfg, v)

	// Start request 1 which occupies the single slot on "slow"
	go func() {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+raw)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
	}()

	<-inFlight // Wait for request 1 to be inside "slow"

	// Request 2 hits combo "auto": "slow" has max_concurrency 1 reached -> falls back to "fast"!
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
	req2.Header.Set("Authorization", "Bearer "+raw)
	rr2 := httptest.NewRecorder()
	router.ServeHTTP(rr2, req2)

	close(done)

	if rr2.Code != http.StatusOK || rr2.Body.String() != `{"provider":"fast"}` {
		t.Fatalf("expected fallback to fast when slow concurrency limit reached, got status %d body %s", rr2.Code, rr2.Body.String())
	}
}

// Direct (non-combo) dispatch has no failover, so gateway backpressure must
// reach the client as 429 with Retry-After, not as a 502 upstream error.
func TestDirectProviderSaturationReturns429(t *testing.T) {
	inFlight := make(chan struct{})
	done := make(chan struct{})
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(_ context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
			inFlight <- struct{}{}
			<-done
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return nil
		}}
	})

	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "solo", Type: "openai", BaseURL: "https://solo", Models: []string{"m1"}, MaxConcurrency: 1},
		},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	router := testRouter(t, cfg, v)

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+raw)
		router.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-inFlight

	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[]}`))
	req2.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req2)
	close(done)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %q", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got == "" {
		t.Fatal("expected Retry-After header on saturation")
	}
	if !strings.Contains(rr.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("body = %q, want rate_limit_exceeded code", rr.Body.String())
	}
}

func TestUpstreamRateLimitReturns429WithRetryAfter(t *testing.T) {
	rr := httptest.NewRecorder()
	writeProviderError(rr, upstreamRateLimitError{})

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %q", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
	if !strings.Contains(rr.Body.String(), `"code":"rate_limit_exceeded"`) {
		t.Fatalf("body = %q, want rate_limit_exceeded", rr.Body.String())
	}
}

// When every target in a combo is locally saturated there is no failover left,
// so the client must get 429 + Retry-After rather than a misleading 502.
func TestComboFullySaturatedReturns429(t *testing.T) {
	inFlight := make(chan struct{})
	done := make(chan struct{})
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(_ context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
			inFlight <- struct{}{}
			<-done
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return nil
		}}
	})

	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "a", Type: "openai", Models: []string{"m1"}, MaxConcurrency: 1},
			{Name: "b", Type: "openai", Models: []string{"m2"}, MaxConcurrency: 1},
		},
		Combos: []config.Combo{{
			Name: "safe", Strategy: "reliable",
			Targets: []config.ComboTarget{{Provider: "a", Model: "m1"}, {Provider: "b", Model: "m2"}},
		}},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	router := testRouter(t, cfg, v)

	// Occupy the single slot on both targets.
	for range 2 {
		go func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"safe","messages":[]}`))
			req.Header.Set("Authorization", "Bearer "+raw)
			router.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	<-inFlight
	<-inFlight

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"safe","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	close(done)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %q", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got == "" {
		t.Fatal("expected Retry-After header when every target is saturated")
	}
}

// A client that disconnects mid-stream cancels the request context. That is not
// an upstream fault, so the target must not be drained: otherwise a few aborts
// knock a healthy model out of the combo for the whole drain TTL.
func TestComboClientAbortMidStreamDoesNotDrain(t *testing.T) {
	started := make(chan struct{})
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(ctx context.Context, _ provider.ChatRequest, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
			w.(http.Flusher).Flush() // commit: the client is now mid-stream
			close(started)
			<-ctx.Done() // client abort surfaces here
			return ctx.Err()
		}}
	})
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "a", Type: "openai", Models: []string{"m1"}}},
		Combos: []config.Combo{{
			Name: "safe", Strategy: "reliable", DrainTTL: "1m",
			Targets: []config.ComboTarget{{Provider: "a", Model: "m1"}},
		}},
	}
	tr := combo.NewTracker("")
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	router := NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, tr, "test-version", nil)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"safe","messages":[],"stream":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+raw)
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-started
	cancel()
	<-done

	if tr.IsDrained(combo.Target{Provider: "a", Model: "m1"}) {
		t.Fatalf("target drained after client abort: %q", tr.DrainReason(combo.Target{Provider: "a", Model: "m1"}))
	}
}

// The counterpart to the abort test: a genuinely stalled upstream leaves the
// request context alive, so it must still be drained and failed over.
func TestComboUpstreamStallAfterCommitStillDrains(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(_ context.Context, _ provider.ChatRequest, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
			w.(http.Flusher).Flush() // commit
			return provider.ErrUpstreamStall
		}}
	})
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "a", Type: "openai", Models: []string{"m1"}}},
		Combos: []config.Combo{{
			Name: "safe", Strategy: "reliable", DrainTTL: "1m",
			Targets: []config.ComboTarget{{Provider: "a", Model: "m1"}},
		}},
	}
	tr := combo.NewTracker("")
	performChat(t, cfg, tr, `{"model":"safe","messages":[],"stream":true}`)

	target := combo.Target{Provider: "a", Model: "m1"}
	if !tr.IsDrained(target) {
		t.Fatal("stalled upstream must still be drained after commit")
	}
}

// A direct (non-combo) dispatch failure must not echo upstream error text that
// embeds a credential.
func TestDirectProviderErrorIsSanitized(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(context.Context, provider.ChatRequest, http.ResponseWriter) error {
			return errors.New("upstream rejected Authorization: Bearer sk-example-redaction-fixture")
		}}
	})
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}}},
	}
	rr := performChat(t, cfg, combo.NewTracker(""), `{"model":"gpt-4o","messages":[]}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "sk-example-redaction-fixture") {
		t.Fatalf("direct path leaked credential: %q", rr.Body.String())
	}
}

// Every combo strategy routes through the same buffered writer, so a target
// that fails before committing must leave no partial bytes in the response and
// the next target must still deliver a clean stream.
func TestAllStrategiesBufferUntilCommit(t *testing.T) {
	for _, strategy := range []string{"priority", "fill-first", "reliable", "round-robin"} {
		t.Run(strategy, func(t *testing.T) {
			var calls []string
			provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
				return &responseProvider{name: cfg.Name, chat: func(_ context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
					calls = append(calls, cfg.Name)
					if cfg.Name == "bad" {
						// Partial SSE bytes then a failure: nothing may reach the client.
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"bad\"}}]}\n\n"))
						return io.ErrUnexpectedEOF
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"good\"}}]}\n\n"))
					w.(http.Flusher).Flush()
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
					return nil
				}}
			})
			cfg := reliableTestConfig()
			cfg.Combos[0].Strategy = strategy
			tr := combo.NewTracker("")
			rr := performChat(t, cfg, tr, `{"model":"safe","stream":true,"messages":[]}`)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %q", rr.Code, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), `"content":"bad"`) {
				t.Fatalf("partial bytes from the failed target leaked: %q", rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), `"content":"good"`) || !strings.Contains(rr.Body.String(), "[DONE]") {
				t.Fatalf("fallback target did not deliver a clean stream: %q", rr.Body.String())
			}
			if len(calls) != 2 {
				t.Fatalf("target calls = %v, want both targets tried", calls)
			}
		})
	}
}

// Upstream errors frequently carry credentials inside a URL query string or a
// JSON blob, where whitespace tokenization cannot isolate them. Redaction must
// cover those shapes too, or a raw upstream error leaks a key to the client.
func TestSanitizeFailureRedactsEmbeddedCredentials(t *testing.T) {
	cases := []string{
		`Post "https://api.example.com/v1?api_key=sk-embedded-fixture": dial tcp: timeout`,
		`Get "https://api.example.com/v1?token=sk-embedded-fixture": EOF`,
		`Get "https://api.example.com/v1?apikey=sk-embedded-fixture": EOF`,
		`Get "https://api.example.com/v1?key=sk-embedded-fixture": EOF`,
		`Get "https://api.example.com/v1?secret=sk-embedded-fixture": EOF`,
		`Get "https://user:sk-embedded-fixture@api.example.com/v1": EOF`,
		`{"error":"invalid","api_key":"sk-embedded-fixture"}`,
	}
	for _, in := range cases {
		if out := sanitizeFailure(errors.New(in)); strings.Contains(out, "sk-embedded-fixture") {
			t.Errorf("credential survived redaction:\n  in : %s\n  out: %s", in, out)
		}
	}
}
