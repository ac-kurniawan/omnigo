package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func testRouter(t *testing.T, cfg *config.Config, v *vault.Vault) http.Handler {
	return NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(v), func(fn func(*config.Config) error) error {
		return fn(cfg)
	})
}

func TestModelsUnauthorized(t *testing.T) {
	cfg := &config.Config{}
	v := &vault.Vault{}
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, httptest.NewRequest("GET", "/v1/models", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestModelsAuthorizedListsComboAndProviders(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{"gpt-4o"}}},
		Combos:    []config.Combo{{Name: "auto", Strategy: "priority", Targets: []config.ComboTarget{{Provider: "openai", Model: "gpt-4o"}}}},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !containsStr(body, "auto") || !containsStr(body, "openai/gpt-4o") {
		t.Fatalf("body = %s", body)
	}
}

func TestChatCompletionsAuthRequired(t *testing.T) {
	rr := httptest.NewRecorder()
	testRouter(t, &config.Config{}, &vault.Vault{}).ServeHTTP(rr, httptest.NewRequest("POST", "/v1/chat/completions", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
