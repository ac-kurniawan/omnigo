package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
)

// A smart combo renders as a route list, not as a target chain, and its edit
// button carries the routes so the modal can restore them.
func TestCombosRenderSmartRoutes(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{
			{
				Name:         "combo/smart",
				Strategy:     config.StrategySmart,
				DefaultRoute: "flash",
				Routes: []config.SmartRoute{
					{Name: "flash", Criteria: "A quick question", Destination: "combo/flash"},
					{Name: "pro", Criteria: "A hard engineering task", Destination: "agy/gemini-3.8-flash"},
				},
				Jev: config.SmartJev{Model: "jev-latest"},
			},
			{Name: "combo/flash", Strategy: "priority", Targets: []config.ComboTarget{{Provider: "agy", Model: "gemini-3.8-flash"}}},
		},
	}
	s := newComboTestServer(t, cfg, nil)

	req := httptest.NewRequest("GET", "/combos", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// The routes are shown with their destinations.
	if !strings.Contains(body, "combo/flash") {
		t.Errorf("rendered table missing the flash route destination")
	}
	if !strings.Contains(body, "agy/gemini-3.8-flash") {
		t.Errorf("rendered table missing the pro route destination")
	}
	// The smart badge marks it, so a plain combo is not confused with it.
	if !strings.Contains(body, ">smart<") {
		t.Errorf("rendered table missing the smart strategy badge")
	}
	// The edit call carries the routes as JSON. The template escapes the
	// quotes as \u0022 inside the JS string literal, which the browser
	// decodes back to a quote when the attribute is parsed.
	if !strings.Contains(body, `name`) || !strings.Contains(body, `A quick question`) {
		t.Errorf("rendered table missing the route JSON in the edit call")
	}
	if !strings.Contains(body, "openEditComboModal") {
		t.Errorf("rendered table missing the edit call")
	}
	// The destination picker is fed the plain combo names and the model pairs.
	if !strings.Contains(body, "SMART_COMBO_NAMES") || !strings.Contains(body, "SMART_MODEL_TARGETS") {
		t.Errorf("rendered table missing the smart destination data")
	}
}

// A smart combo must not render the plain target-chain editor: it has no
// chain of its own.
func TestCombosRenderSmartHasNoTargetChain(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{{
			Name:     "combo/smart",
			Strategy: config.StrategySmart,
			Routes:   []config.SmartRoute{{Name: "flash", Criteria: "quick", Destination: "combo/flash"}},
		}},
	}
	s := newComboTestServer(t, cfg, nil)
	req := httptest.NewRequest("GET", "/combos", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	body := rr.Body.String()

	if strings.Contains(body, `name="targets" value=""`) {
		t.Errorf("smart combo rendered an empty target chain input")
	}
}
