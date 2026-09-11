package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func testHandler(t *testing.T, cfg *config.Config, v *vault.Vault) http.Handler {
	return NewHandler(func() *config.Config { return cfg }, vault.NewMemoryStore(v), func(fn func(*config.Config) error) error {
		return fn(cfg)
	}, nil)
}

func TestIndexRendersProvidersCombosAndKeys(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{"gpt-4o"}}},
		Combos:    []config.Combo{{Name: "auto", Strategy: "priority"}},
	}
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", Name: "dev", Prefix: "ak-12345678", Active: true}}}
	h := testHandler(t, cfg, v)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "openai") || !strings.Contains(body, "auto") || !strings.Contains(body, "ak-12345678") {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(body, "copyToClipboard") {
		t.Fatalf("body missing copyToClipboard function")
	}
	if strings.Contains(body, "onclick=\"navigator.clipboard") {
		t.Fatalf("body contains raw onclick navigator.clipboard which fails in insecure contexts")
	}
}

func TestServesStaticHtmx(t *testing.T) {
	cfg := &config.Config{}
	h := testHandler(t, cfg, &vault.Vault{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/static/htmx.min.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "htmx") {
		t.Fatalf("body does not contain htmx: %s", rr.Body.String()[:100])
	}
}
