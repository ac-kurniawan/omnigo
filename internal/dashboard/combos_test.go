package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestAddCombo(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "openai-main", Type: "openai"},
			{Name: "agy", Type: "antigravity"},
		},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		return fn(cfg)
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate)

	req := httptest.NewRequest("POST", "/combos", strings.NewReader("name=smart&strategy=priority&targets=agy/gemini-3.7-flash,openai-main/gpt-4o"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(cfg.Combos) != 1 || cfg.Combos[0].Name != "smart" {
		t.Fatalf("combos = %+v", cfg.Combos)
	}
	if len(cfg.Combos[0].Targets) != 2 || cfg.Combos[0].Targets[0].Provider != "agy" {
		t.Fatalf("targets = %+v", cfg.Combos[0].Targets)
	}
	if !strings.Contains(rr.Body.String(), "smart") {
		t.Fatalf("body missing smart: %s", rr.Body.String())
	}
}

func TestDeleteCombo(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{{Name: "smart", Strategy: "priority"}},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		return fn(cfg)
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/combos/smart/delete", nil)
	req.Header.Set("HX-Request", "true")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(cfg.Combos) != 0 {
		t.Fatalf("expected empty combos, got %+v", cfg.Combos)
	}
}
