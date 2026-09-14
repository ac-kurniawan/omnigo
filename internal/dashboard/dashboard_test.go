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

func TestIndexRendersCodexConnectionStateAndActions(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex", Models: []string{"gpt-6-astra"}}}}
	v := &vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{"codex-main": {RefreshToken: "stored", AccountID: "workspace-test", Email: "user@example.com"}}}
	rr := httptest.NewRecorder()
	testHandler(t, cfg, v).ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	body := rr.Body.String()
	for _, value := range []string{"Connect ChatGPT", "user@example.com", "Workspace: workspace-test", "/internal/test/codex-main", "/internal/refresh-models/codex-main"} {
		if !strings.Contains(body, value) {
			t.Fatalf("dashboard missing %q", value)
		}
	}
}

func TestIndexRendersOAuthAccountPoolActions(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex"}}}
	v := &vault.Vault{ProviderAccounts: map[string][]vault.ProviderSecret{
		"codex-main": {
			{RefreshToken: "refresh-1", AccountID: "workspace-1", Email: "one@example.com"},
			{RefreshToken: "refresh-2", AccountID: "workspace-2", Email: "two@example.com"},
		},
	}}
	rr := httptest.NewRecorder()
	testHandler(t, cfg, v).ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	body := rr.Body.String()
	for _, value := range []string{"one@example.com", "two@example.com", "Connect Another", "/providers/codex-main/accounts/workspace-1/delete", "Reconnect"} {
		if !strings.Contains(body, value) {
			t.Fatalf("dashboard missing %q", value)
		}
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
