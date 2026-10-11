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

// A target chosen from a synthesised effort clone must survive the whole path:
// the picker's data-target value, the create-combo form, config validation, and
// the stored combo. This is the payload the browser actually submits.
func TestCreateComboWithAnEffortCloneTarget(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			// Only the "-low" variant is cached, so "-high" is a clone.
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
		"targets":  {"agy/gemini-3.1-pro-high"},
	}
	req := httptest.NewRequest("POST", "/combos", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(cfg.Combos) != 1 {
		t.Fatalf("combos = %+v, want the combo to be stored", cfg.Combos)
	}
	got := cfg.Combos[0].Targets
	if len(got) != 1 || got[0].Provider != "agy" || got[0].Model != "gemini-3.1-pro-high" {
		t.Fatalf("targets = %+v, want the suffixed model preserved verbatim", got)
	}
	if !strings.Contains(rr.Body.String(), "gemini-3.1-pro-high") {
		t.Fatalf("re-rendered combos missing the target: %s", rr.Body.String())
	}
}
