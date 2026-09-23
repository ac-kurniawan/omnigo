package dashboard

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/quota"
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

func TestIndexRendersOAuthPoolAccordionWithHealthyCount(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{
		{Name: "agy", Type: "antigravity"},
		{Name: "cx", Type: "codex"},
	}}
	v := &vault.Vault{ProviderAccounts: map[string][]vault.ProviderSecret{
		"agy": {
			{RefreshToken: "rt-1", AccountID: "google-1", Email: "one@example.com"},
			{AccountID: "google-2", Email: "dead@example.com"},
			{RefreshToken: "rt-3", AccountID: "google-3", Email: "three@example.com"},
		},
		"cx": {
			{RefreshToken: "rt-1", AccountID: "ws-1", Email: "cx-one@example.com"},
		},
	}}
	rr := httptest.NewRecorder()
	testHandler(t, cfg, v).ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	body := rr.Body.String()

	// Count badge next to the status dot: healthy/registered per provider.
	for _, want := range []string{"2/3 connected", "1/1 connected"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing count badge %q", want)
		}
	}

	// Account pool must be collapsed by default: the accordion checkbox must
	// not carry a checked attribute. Two OAuth providers -> two accordions.
	opens := strings.Count(body, `collapse-arrow accounts-collapse`)
	if opens != 2 {
		t.Fatalf("expected 2 account-pool accordions, got %d", opens)
	}

	for _, frag := range strings.Split(body, "\n") {
		if strings.Contains(frag, "accounts-collapse") && strings.Contains(frag, "checked") {
			t.Fatalf("accordion checkbox must not be checked by default: %s", frag)
		}
	}
	if !strings.Contains(body, `data-accordion="agy"`) || !strings.Contains(body, `data-accordion="cx"`) {
		t.Fatal("accordion must be keyed by provider so open state survives a partial re-render")
	}
	// Open state is remembered in the browser for the session, then reapplied
	// after an HTMX swap of #providers. The server response itself stays closed.
	for _, want := range []string{
		"omnigo-open-accordions",
		"htmx:afterSwap",
		"data-accordion-toggle",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing accordion persistence hook %q", want)
		}
	}
}

func TestCollapseArrowChevronVerticallyCentered(t *testing.T) {
	h := testHandler(t, &config.Config{}, &vault.Vault{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()

	// daisyUI 4 pins the collapse-arrow chevron at a fixed top: 1.9rem with
	// translateY(-100%), tuned for the default 4rem title. Our titles are
	// shorter (py-1.5 / min-h-0), so the chevron overflows below the header
	// and overlaps the content. The override must re-anchor it to the title's
	// vertical center (top: 50% + translateY(-50%)).
	if !strings.Contains(body, ".collapse-arrow > .collapse-title::after { top: 50%; --tw-translate-y: -50%; }") {
		t.Fatalf("missing chevron centering override for collapse-title::after")
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

func TestDashboardDisabledReturns404(t *testing.T) {
	off := false
	cfg := &config.Config{Dashboard: config.Dashboard{Enabled: &off}}
	h := NewHandler(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)

	for _, path := range []string{"/", "/providers", "/keys", "/oauth/login/codex-main", "/static/htmx.min.js"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", path, rr.Code)
		}
	}
}

func TestDashboardDisableTakesEffectWithoutRestart(t *testing.T) {
	enabled, authOff := true, false
	cfg := &config.Config{Dashboard: config.Dashboard{Enabled: &enabled, Auth: &authOff}}
	h := NewHandler(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("enabled: status = %d, want 200", rr.Code)
	}

	enabled = false
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled: status = %d, want 404", rr.Code)
	}

	enabled = true
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("re-enabled: status = %d, want 200", rr.Code)
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
		"Playground &amp; Docs",
		"showTab('quickstart'",
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

// A template execution error truncates the providers partial mid-render: the
// HTTP status is already 200, so the page silently loses every account after
// the failure. Guard the whole partial, including the grouped quota bars and
// detail modals, so a bad template fails the build rather than production.
func TestProvidersPartialRendersGroupedAntigravityQuota(t *testing.T) {
	weeklyReset := time.Now().Add(17*time.Hour + 43*time.Minute)
	fiveHourReset := time.Now().Add(4 * time.Hour)
	cache := quota.NewCache()
	cache.Put(quota.AccountSnapshot{
		Provider:   "agy",
		Identity:   "sub-1:user@example.com",
		Email:      "user@example.com",
		Status:     quota.StatusExhausted,
		Reason:     "Claude and GPT models · Weekly limit",
		ObservedAt: time.Now().Add(-90 * time.Minute),
		Groups: []quota.Group{
			{
				Name:        "Gemini Models",
				Description: "Models within this group: Gemini Flash, Gemini Pro",
				Windows: []quota.Window{
					{Name: "Gemini Models", UsedPercent: 14.36, WindowMinutes: 10080, ResetAt: weeklyReset},
					{Name: "Gemini Models", UsedPercent: 0, WindowMinutes: 300, ResetAt: fiveHourReset},
				},
			},
			{
				Name:        "Claude and GPT models",
				Description: "Models within this group: Claude Opus, Claude Sonnet, GPT-OSS",
				Windows: []quota.Window{
					{Name: "Claude and GPT models", UsedPercent: 100, WindowMinutes: 10080, ResetAt: weeklyReset},
					{Name: "Claude and GPT models", UsedPercent: 25, WindowMinutes: 300, ResetAt: fiveHourReset},
				},
			},
		},
	})
	disabled := false
	cfg := &config.Config{
		Dashboard: config.Dashboard{Auth: &disabled},
		Providers: []config.Provider{{Name: "agy", Type: "antigravity"}},
	}
	v := &vault.Vault{ProviderAccounts: map[string][]vault.ProviderSecret{
		"agy": {{AccountID: "sub-1", Email: "user@example.com"}},
	}}
	h := NewHandlerWithQuota(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, cache)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/providers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		">Exhausted<",
		`class="modal"`,
		"Gemini Models",
		"Claude and GPT models",
		"Models within this group: Gemini Flash, Gemini Pro",
		// Inline bars use the short label; the group name is in the aria-label.
		`aria-label="Gemini Models · Weekly limit remaining quota"`,
		`aria-label="Gemini Models · 5-hour limit remaining quota"`,
		`aria-label="Claude and GPT models · Weekly limit remaining quota"`,
		`aria-label="Claude and GPT models · 5-hour limit remaining quota"`,
		"85.6%", // a fractional remaining percent must not abort rendering
		"75%",
		`class="progress progress-error`,
		"Observed 1h ago",
		"Refreshes",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("providers partial missing %q (render truncated?)", want)
		}
	}
	// The hidden weekly limit is now reported, so the old "not reported by
	// Antigravity" disclaimer must be gone.
	for _, gone := range []string{"Weekly limit is not reported", "quota shown per model"} {
		if strings.Contains(body, gone) {
			t.Fatalf("providers partial still claims %q", gone)
		}
	}
	if strings.Contains(body, "{{") {
		t.Fatalf("unrendered template action left in output")
	}
}

// A grouped snapshot renders both groups inline and in the modal, and the group
// order follows upstream rather than being sorted away.
func TestProvidersPartialGroupedQuotaKeepsBothGroups(t *testing.T) {
	cache := quota.NewCache()
	cache.Put(quota.AccountSnapshot{
		Provider:   "agy",
		Identity:   "sub-1:user@example.com",
		Status:     quota.StatusAvailable,
		ObservedAt: time.Now(),
		Groups: []quota.Group{
			{Name: "Gemini Models", Windows: []quota.Window{
				{Name: "Gemini Models", UsedPercent: 10, WindowMinutes: 10080},
				{Name: "Gemini Models", UsedPercent: 20, WindowMinutes: 300},
			}},
			{Name: "Claude and GPT models", Windows: []quota.Window{
				{Name: "Claude and GPT models", UsedPercent: 30, WindowMinutes: 10080},
				{Name: "Claude and GPT models", UsedPercent: 40, WindowMinutes: 300},
			}},
		},
	})
	disabled := false
	cfg := &config.Config{
		Dashboard: config.Dashboard{Auth: &disabled},
		Providers: []config.Provider{{Name: "agy", Type: "antigravity"}},
	}
	v := &vault.Vault{ProviderAccounts: map[string][]vault.ProviderSecret{
		"agy": {{AccountID: "sub-1", Email: "user@example.com", RefreshToken: "rt"}},
	}}
	h := NewHandlerWithQuota(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, cache)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/providers", nil))
	body := rr.Body.String()

	if n := strings.Count(body, `class="progress `); n != 8 {
		t.Fatalf("progress bar count = %d, want 8 (4 inline + 4 in the modal)", n)
	}
	inline := body[strings.Index(body, "flex flex-col gap-2.5 pt-0.5"):]
	inline = inline[:strings.Index(inline, "<dialog")]
	gemini := strings.Index(inline, "Gemini Models")
	claude := strings.Index(inline, "Claude and GPT models")
	if gemini < 0 || claude < 0 || gemini > claude {
		t.Fatalf("inline groups out of upstream order: %s", inline)
	}
}

func TestProvidersPartialUsesReadableCodexQuotaLabelsAndStatusPills(t *testing.T) {
	cache := quota.NewCache()
	identity := "acct:user@example.com"
	cache.Put(quota.AccountSnapshot{
		Provider:   "cx",
		Identity:   identity,
		Status:     quota.StatusAvailable,
		ObservedAt: time.Now(),
		Windows: []quota.Window{
			{Name: "primary", UsedPercent: 7, WindowMinutes: 300},
			{Name: "secondary", UsedPercent: 20, WindowMinutes: 10080},
		},
	})
	disabled := false
	cfg := &config.Config{
		Dashboard: config.Dashboard{Auth: &disabled},
		Providers: []config.Provider{{Name: "cx", Type: "codex"}},
	}
	v := &vault.Vault{ProviderAccounts: map[string][]vault.ProviderSecret{
		"cx": {{AccountID: "acct", Email: "user@example.com", RefreshToken: "rt"}},
	}}
	h := NewHandlerWithQuota(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, cache)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/providers", nil))
	body := rr.Body.String()
	for _, want := range []string{
		">5-hour limit<",
		">Weekly limit<",
		">Ready<",
		">Available<",
		"status-pill status-pill-success",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("providers partial missing %q", want)
		}
	}
	for _, old := range []string{">primary<", ">secondary<", ">ready<", ">Quota OK<"} {
		if strings.Contains(body, old) {
			t.Errorf("providers partial still contains raw label %q", old)
		}
	}
}

// Colour is the only signal that a bar is nearly drained when the table is
// skimmed, so pin the thresholds independently of the markup.
func TestQuotaWindowProgressClassThresholds(t *testing.T) {
	s := newServer(func() *config.Config { return &config.Config{} }, vault.NewMemoryStore(&vault.Vault{}), nil)
	probe, err := s.tmpl.New("probe").Parse(`{{quotaWindowProgressClass .}}`)
	if err != nil {
		t.Fatalf("parse probe: %v", err)
	}
	class := func(used float64) string {
		var buf bytes.Buffer
		if err := probe.Execute(&buf, quota.Window{UsedPercent: used}); err != nil {
			t.Fatalf("render probe for used=%v: %v", used, err)
		}
		return buf.String()
	}

	for _, tc := range []struct {
		used float64
		want string
	}{
		{0, "progress-success"},
		{69.9, "progress-success"},
		{70, "progress-warning"},
		{90, "progress-warning"},
		{90.1, "progress-error"},
		{100, "progress-error"},
		{150, "progress-error"}, // clamped remaining must still read as drained
	} {
		if got := class(tc.used); got != tc.want {
			t.Errorf("used=%v: class = %q, want %q", tc.used, got, tc.want)
		}
	}
}
