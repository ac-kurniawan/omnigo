package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/auth"
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

func TestProviderFallsBackToConfigAPIKey(t *testing.T) {
	var gotKey string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		gotKey = store.Get().APIKey
		return &fakeProvider{name: cfg.Name}
	})
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x", APIKey: "sk-from-config"}},
	}
	v := &vault.Vault{}
	_, _ = buildProvider(cfg.Providers[0], vault.NewMemoryStore(v))
	if gotKey != "sk-from-config" {
		t.Fatalf("gotKey = %q, want sk-from-config", gotKey)
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
