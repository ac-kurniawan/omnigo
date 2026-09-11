package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestInternalEndpointsRequireAuthentication(t *testing.T) {
	for _, path := range []string{"/internal/refresh-models/openai", "/internal/test/openai"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			testRouter(t, &config.Config{}, &vault.Vault{}).ServeHTTP(rr, httptest.NewRequest("POST", path, nil))
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rr.Code)
			}
		})
	}
}

func TestInternalRefreshModels(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, models: []provider.Model{{ID: "gpt-4o"}, {ID: "gpt-4o-mini"}}}
	})
	cfg := &config.Config{Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x"}}}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	req := httptest.NewRequest("POST", "/internal/refresh-models/openai", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "gpt-4o") {
		t.Fatalf("body = %s", rr.Body.String())
	}
	// persistence: the provider's cached model list is updated in-memory
	if len(cfg.Providers[0].Models) != 2 || cfg.Providers[0].Models[0] != "gpt-4o" {
		t.Fatalf("models not persisted: %+v", cfg.Providers[0].Models)
	}
}

func TestInternalTestProvider(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name}
	})
	cfg := &config.Config{Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x"}}}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	req := httptest.NewRequest("POST", "/internal/test/openai", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}
