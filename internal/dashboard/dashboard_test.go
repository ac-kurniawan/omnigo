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
	disabled := false
	if cfg.Dashboard.Auth == nil {
		cfg.Dashboard.Auth = &disabled
	}
	return NewHandler(func() *config.Config { return cfg }, vault.NewMemoryStore(v), func(fn func(*config.Config) error) error {
		return fn(cfg)
	}, nil)
}

func TestFallbackCopyUsesDialogContainer(t *testing.T) {
	h := testHandler(t, &config.Config{}, &vault.Vault{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rr.Body.String()
	for _, want := range []string{
		"fallbackCopy(text, btn, showSuccess)",
		"const container = btn.closest('dialog') || document.body;",
		"container.appendChild(textarea);",
		"textarea.focus();",
		"textarea.setSelectionRange(0, textarea.value.length);",
		"container.removeChild(textarea);",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing dialog-aware fallback copy code %q", want)
		}
	}
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
	if !strings.Contains(body, "X-Omnigo-CSRF") {
		t.Fatal("body missing dashboard CSRF request header configuration")
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "omnigo_csrf" || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("csrf cookies = %+v", cookies)
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
	for _, value := range []string{"one@example.com", "two@example.com", "Connect Another", "/providers/codex-main/accounts/workspace-1:one@example.com/delete", "Reconnect"} {
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

func TestDashboardAuthEnabledByDefaultRejectsUnauthenticated(t *testing.T) {
	cfg := &config.Config{}
	h := NewHandler(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if authHeader := rr.Header().Get("WWW-Authenticate"); authHeader != `Basic realm="OmniGo Dashboard"` {
		t.Fatalf("WWW-Authenticate = %q, want Basic realm=\"OmniGo Dashboard\"", authHeader)
	}
}

func TestDashboardAuthAllowsDefaultAdminAdmin(t *testing.T) {
	t.Setenv("OMNIGO_DASH_USER", "")
	t.Setenv("OMNIGO_DASH_PASS", "")

	cfg := &config.Config{}
	h := NewHandler(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "admin")
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestDashboardAuthCustomCredentials(t *testing.T) {
	t.Setenv("OMNIGO_DASH_USER", "operator")
	t.Setenv("OMNIGO_DASH_PASS", "supersecret")

	cfg := &config.Config{}
	h := NewHandler(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)

	// admin/admin should be rejected
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "admin")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("admin/admin status = %d, want 401", rr.Code)
	}

	// custom credentials should succeed
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("operator", "supersecret")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("custom credentials status = %d, want 200", rr.Code)
	}
}

func TestDashboardAuthDisabledInConfig(t *testing.T) {
	disabled := false
	cfg := &config.Config{
		Dashboard: config.Dashboard{Auth: &disabled},
	}
	h := NewHandler(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestDashboardAuthDynamicToggle(t *testing.T) {
	authVal := true
	cfg := &config.Config{
		Dashboard: config.Dashboard{Auth: &authVal},
	}
	h := NewHandler(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)

	// 1. Auth is enabled: unauthenticated request is 401
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("enabled auth: status = %d, want 401", rr.Code)
	}

	// 2. Auth is toggled to disabled: unauthenticated request is 200
	authVal = false
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("disabled auth: status = %d, want 200", rr.Code)
	}

	// 3. Auth is toggled back to enabled: unauthenticated request is 401
	authVal = true
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("re-enabled auth: status = %d, want 401", rr.Code)
	}
}

func TestIndexRendersPlaygroundInspector(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "antigravity", Type: "antigravity", Models: []string{"gemini-2.5-pro"}}},
		Combos:    []config.Combo{{Name: "auto", Strategy: "priority"}},
	}
	v := &vault.Vault{}
	h := testHandler(t, cfg, v)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"Gateway Inspector &amp; Playground",
		"id=\"playground-key\"",
		"id=\"playground-model\"",
		"id=\"playground-prompt\"",
		"id=\"playground-send-btn\"",
		"id=\"playground-abort-btn\"",
		"id=\"telemetry-traceid\"",
		"id=\"telemetry-ttft\"",
		"50,000",
		"200,000",
		"sessionStorage.getItem('omnigo_playground_key')",
		"fetch('/v1/chat/completions'",
		"traceparent",
		"antigravity/gemini-2.5-pro",
		"auto (strategy: priority)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing expected playground element: %q", want)
		}
	}
}

func TestKeysRendersUseInPlaygroundAction(t *testing.T) {
	cfg := &config.Config{}
	v := &vault.Vault{}
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil)
	rr := httptest.NewRecorder()
	s.renderKeys(rr, "ak-test-plain-secret-key")

	body := rr.Body.String()
	if !strings.Contains(body, "useKeyInPlayground('ak-test-plain-secret-key')") {
		t.Errorf("renderKeys missing useKeyInPlayground button call, got: %s", body)
	}
	if !strings.Contains(body, "Use in Playground") {
		t.Errorf("renderKeys missing 'Use in Playground' text, got: %s", body)
	}
}
