package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// smartComboConfig is a config with one plain combo and one smart combo whose
// routes point at the plain combo.
func smartComboConfig() *config.Config {
	return &config.Config{
		Providers: []config.Provider{{Name: "agy", Type: "openai", BaseURL: "https://x", Models: []string{"gemini-3.8-flash"}}},
		Combos: []config.Combo{
			{Name: "combo/flash", Strategy: "priority", Targets: []config.ComboTarget{{Provider: "agy", Model: "gemini-3.8-flash"}}},
			{
				Name:         "combo/smart",
				Strategy:     config.StrategySmart,
				DefaultRoute: "flash",
				Routes: []config.SmartRoute{
					{Name: "flash", Criteria: "A quick factual question", Destination: "combo/flash"},
					{Name: "pro", Criteria: "A hard multi-step engineering task", Destination: "agy/gemini-3.8-flash"},
				},
				Jev: config.SmartJev{Model: "jev-latest", MinConfidence: "0.5"},
			},
		},
	}
}

func newSmartDashboard(t *testing.T, cfg *config.Config) http.Handler {
	t.Helper()
	disabled := false
	cfg.Dashboard.Auth = &disabled
	store := vault.NewMemoryStore(&vault.Vault{})
	return NewHandler(func() *config.Config { return cfg }, store, func(fn func(*config.Config) error) error {
		return fn(cfg)
	})
}

// The create form stores a smart combo with its routes.
func TestCreateSmartComboStoresRoutes(t *testing.T) {
	cfg := smartComboConfig()
	form := url.Values{
		"name":               {"combo/new-smart"},
		"strategy":           {config.StrategySmart},
		"default_route":      {"flash"},
		"routes":             {"flash|A quick question|combo/flash\npro|A hard task|agy/gemini-3.8-flash"},
		"jev_model":          {"jev-latest"},
		"jev_min_confidence": {"0.5"},
	}
	req := httptest.NewRequest("POST", "/combos", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	newSmartDashboard(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	found, ok := config.FindCombo(cfg, "combo/new-smart")
	if !ok {
		t.Fatal("smart combo was not created")
	}
	if len(found.Routes) != 2 {
		t.Fatalf("routes = %+v", found.Routes)
	}
	if found.Routes[0].Name != "flash" || found.Routes[0].Criteria != "A quick question" || found.Routes[0].Destination != "combo/flash" {
		t.Errorf("route 0 = %+v", found.Routes[0])
	}
	if found.Routes[1].Destination != "agy/gemini-3.8-flash" {
		t.Errorf("route 1 = %+v", found.Routes[1])
	}
	if found.DefaultRoute != "flash" {
		t.Errorf("default route = %q", found.DefaultRoute)
	}
	if found.Jev.Model != "jev-latest" || found.Jev.MinConfidence != "0.5" {
		t.Errorf("jev = %+v", found.Jev)
	}
}

// A smart combo created with a default route that is not one of its routes is
// rejected, so a fallback always resolves.
func TestCreateSmartComboRejectsUnknownDefaultRoute(t *testing.T) {
	cfg := smartComboConfig()
	form := url.Values{
		"name":          {"combo/bad"},
		"strategy":      {config.StrategySmart},
		"default_route": {"nope"},
		"routes":        {"flash|A quick question|combo/flash"},
	}
	req := httptest.NewRequest("POST", "/combos", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	newSmartDashboard(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if _, ok := config.FindCombo(cfg, "combo/bad"); ok {
		t.Fatal("invalid smart combo was stored")
	}
}

// The edit form replaces the routes of an existing smart combo.
func TestUpdateSmartComboReplacesRoutes(t *testing.T) {
	cfg := smartComboConfig()
	form := url.Values{
		"strategy":      {config.StrategySmart},
		"default_route": {"pro"},
		"routes":        {"pro|Only route now|combo/flash"},
		"jev_model":     {"jev-preview"},
	}
	req := httptest.NewRequest("POST", "/combos/combo/smart", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	newSmartDashboard(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	found, _ := config.FindCombo(cfg, "combo/smart")
	if len(found.Routes) != 1 || found.Routes[0].Name != "pro" {
		t.Fatalf("routes = %+v", found.Routes)
	}
	if found.DefaultRoute != "pro" {
		t.Errorf("default route = %q", found.DefaultRoute)
	}
	if found.Jev.Model != "jev-preview" {
		t.Errorf("jev model = %q", found.Jev.Model)
	}
}

// A plain combo is never edited through the smart form: its chain is kept.
func TestUpdatePlainComboRejectsRoutes(t *testing.T) {
	cfg := smartComboConfig()
	form := url.Values{
		"strategy":      {config.StrategySmart},
		"default_route": {"flash"},
		"routes":        {"flash|A quick question|combo/flash"},
	}
	req := httptest.NewRequest("POST", "/combos/combo/flash", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	newSmartDashboard(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	found, _ := config.FindCombo(cfg, "combo/flash")
	if len(found.Targets) != 1 {
		t.Fatalf("plain combo targets were changed: %+v", found.Targets)
	}
}
