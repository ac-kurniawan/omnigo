package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestInternalRefreshModels(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, models: []provider.Model{{ID: "gpt-4o"}, {ID: "gpt-4o-mini"}}}
	})
	cfg := &config.Config{Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x"}}}
	rr := httptest.NewRecorder()
	testRouter(t, cfg, &vault.Vault{}).ServeHTTP(rr, httptest.NewRequest("POST", "/internal/refresh-models/openai", nil))
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
	rr := httptest.NewRecorder()
	testRouter(t, cfg, &vault.Vault{}).ServeHTTP(rr, httptest.NewRequest("POST", "/internal/test/openai", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}
