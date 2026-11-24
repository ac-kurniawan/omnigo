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

// A target stored as base+effort must survive the whole path: the picker's
// request form, config validation, and the stored combo.
func TestCreateComboWithAnEffortTarget(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			// Only the "-low" variant is cached; the base still resolves to its
			// family, and the other levels are what the dropdown offers.
			{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.1-pro-low"}},
		},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		if err := fn(cfg); err != nil {
			return err
		}
		return cfg.Validate()
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate, nil)

	form := url.Values{
		"name":     {"smart"},
		"strategy": {"priority"},
		"targets":  {"agy/gemini-3.1-pro|high"},
	}
	req := httptest.NewRequest("POST", "/combos", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(cfg.Combos) != 1 || len(cfg.Combos[0].Targets) != 1 {
		t.Fatalf("combos = %+v, want the combo to be stored", cfg.Combos)
	}
	got := cfg.Combos[0].Targets[0]
	if got.Provider != "agy" || got.Model != "gemini-3.1-pro" || got.Effort != "high" {
		t.Fatalf("target = %+v, want the base model with its effort split out", got)
	}
	if !strings.Contains(rr.Body.String(), "gemini-3.1-pro") {
		t.Fatalf("re-rendered combos missing the target: %s", rr.Body.String())
	}
	// The combos table re-posts the same encoding when the strategy changes.
	if !strings.Contains(rr.Body.String(), `value="agy/gemini-3.1-pro|high"`) {
		t.Fatalf("re-rendered combos did not preserve the effort encoding: %s", rr.Body.String())
	}
}

// An effort that is not a concrete level is rejected by name, naming the entry
// and the levels that would work. Silently dropping it would save a shorter
// chain than the operator asked for with no indication.
func TestCreateComboRejectsAnUnparseableEffort(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.1-pro-low"}}},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		if err := fn(cfg); err != nil {
			return err
		}
		return cfg.Validate()
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate, nil)

	form := url.Values{
		"name":     {"smart"},
		"strategy": {"priority"},
		"targets":  {"agy/gemini-3.1-pro|turbo"},
	}
	req := httptest.NewRequest("POST", "/combos", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown effort", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"agy/gemini-3.1-pro|turbo", "turbo", "high"} {
		if !strings.Contains(body, want) {
			t.Errorf("error message missing %q: %s", want, body)
		}
	}
	if len(cfg.Combos) != 0 {
		t.Fatalf("stored %d combos, want none", len(cfg.Combos))
	}
}

// The edit path parses the same encoding, so a typo'd level must be rejected
// there too and leave the stored chain untouched rather than half-applied.
func TestUpdateComboRejectsAnUnparseableEffort(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.1-pro-low"}}},
		Combos: []config.Combo{{Name: "chain", Strategy: "priority", Targets: []config.ComboTarget{
			{Provider: "agy", Model: "gemini-3.1-pro", Effort: "high"},
		}}},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		if err := fn(cfg); err != nil {
			return err
		}
		return cfg.Validate()
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate, nil)

	form := url.Values{
		"strategy": {"priority"},
		"targets":  {"agy/gemini-3.1-pro|turbo"},
	}
	req := httptest.NewRequest("POST", "/combos/chain", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown effort", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "turbo") {
		t.Errorf("error message does not name the bad level: %s", body)
	}
	if got := cfg.Combos[0].Targets; len(got) != 1 || got[0].Effort != "high" {
		t.Fatalf("stored targets = %+v, want the original high-effort target", got)
	}
}
