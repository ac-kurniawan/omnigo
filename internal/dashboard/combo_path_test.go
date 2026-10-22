package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
)

// Combo names in this deployment contain a slash (combo/auto, combo/flash), so
// the edit and delete routes must match more than one path segment. Go's
// ServeMux {name} wildcard matches a single segment, which made every
// slash-named combo impossible to edit or delete from the dashboard.
func TestUpdateComboWithSlashInName(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}}},
		Combos: []config.Combo{{
			Name:     "combo/flash",
			Strategy: "priority",
			Targets:  []config.ComboTarget{{Provider: "openai", Model: "gpt-4o"}},
		}},
	}
	form := url.Values{
		"strategy": {"reliable"},
		"targets":  {"openai/gpt-4o"},
	}
	req := httptest.NewRequest("POST", "/combos/combo/flash", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()

	newSmartDashboard(t, cfg).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	found, ok := config.FindCombo(cfg, "combo/flash")
	if !ok {
		t.Fatal("combo/flash disappeared")
	}
	if found.Strategy != "reliable" {
		t.Errorf("strategy = %q, want reliable", found.Strategy)
	}
}

func TestDeleteComboWithSlashInName(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}}},
		Combos: []config.Combo{{
			Name:     "combo/flash",
			Strategy: "priority",
			Targets:  []config.ComboTarget{{Provider: "openai", Model: "gpt-4o"}},
		}},
	}
	req := httptest.NewRequest("POST", "/combos/combo/flash/delete", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()

	newSmartDashboard(t, cfg).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	if _, ok := config.FindCombo(cfg, "combo/flash"); ok {
		t.Fatal("combo/flash was not deleted")
	}
}

// A percent-encoded slash is what the dashboard's JavaScript sends, because it
// builds the URL with encodeURIComponent. It must reach the same handler.
func TestUpdateComboWithEncodedSlash(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}}},
		Combos: []config.Combo{{
			Name:     "combo/flash",
			Strategy: "priority",
			Targets:  []config.ComboTarget{{Provider: "openai", Model: "gpt-4o"}},
		}},
	}
	form := url.Values{"strategy": {"fill-first"}, "targets": {"openai/gpt-4o"}}
	req := httptest.NewRequest("POST", "/combos/combo%2Fflash", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()

	newSmartDashboard(t, cfg).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	found, _ := config.FindCombo(cfg, "combo/flash")
	if found.Strategy != "fill-first" {
		t.Errorf("strategy = %q, want fill-first", found.Strategy)
	}
}

// A combo whose name itself ends in "/delete" must still be editable: the
// trailing segment is only an action when no such combo exists.
func TestComboNamedWithDeleteSuffixIsStillEditable(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}}},
		Combos: []config.Combo{{
			Name:     "combo/delete",
			Strategy: "priority",
			Targets:  []config.ComboTarget{{Provider: "openai", Model: "gpt-4o"}},
		}},
	}
	form := url.Values{"strategy": {"round-robin"}, "targets": {"openai/gpt-4o"}}
	req := httptest.NewRequest("POST", "/combos/combo/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()

	newSmartDashboard(t, cfg).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	found, ok := config.FindCombo(cfg, "combo/delete")
	if !ok {
		t.Fatal("combo/delete was deleted instead of edited")
	}
	if found.Strategy != "round-robin" {
		t.Errorf("strategy = %q, want round-robin", found.Strategy)
	}
}
