package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/combo"
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
	s := newServer(func() *config.Config { return cfg }, store, mutate, nil)

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

func TestAddComboMultipleTargetsOrdered(t *testing.T) {
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
	s := newServer(func() *config.Config { return cfg }, store, mutate, nil)

	// targets passed as ordered entries
	req := httptest.NewRequest("POST", "/combos", strings.NewReader("name=fast&strategy=priority&targets=agy/m1&targets=openai-main/m2&targets=agy/m3"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(cfg.Combos) != 1 || cfg.Combos[0].Name != "fast" {
		t.Fatalf("combos = %+v", cfg.Combos)
	}
	cb := cfg.Combos[0]
	if len(cb.Targets) != 3 {
		t.Fatalf("expected 3 targets, got %+v", cb.Targets)
	}
	if cb.Targets[0].Provider != "agy" || cb.Targets[0].Model != "m1" {
		t.Fatalf("target[0] = %+v, want agy/m1", cb.Targets[0])
	}
	if cb.Targets[1].Provider != "openai-main" || cb.Targets[1].Model != "m2" {
		t.Fatalf("target[1] = %+v, want openai-main/m2", cb.Targets[1])
	}
	if cb.Targets[2].Provider != "agy" || cb.Targets[2].Model != "m3" {
		t.Fatalf("target[2] = %+v, want agy/m3", cb.Targets[2])
	}
}

func TestResetDrainedTarget(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{{
			Name:     "smart",
			Strategy: "fill-first",
			Targets:  []config.ComboTarget{{Provider: "agy", Model: "m1"}},
		}},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	tr := combo.NewTracker("")
	tr.MarkDrained(combo.Target{Provider: "agy", Model: "m1"}, 1*time.Minute)

	s := newServer(func() *config.Config { return cfg }, store, nil, tr)

	if !tr.IsDrained(combo.Target{Provider: "agy", Model: "m1"}) {
		t.Fatal("expected drained before reset")
	}

	req := httptest.NewRequest("POST", "/combos/drains/reset", strings.NewReader("provider=agy&model=m1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if tr.IsDrained(combo.Target{Provider: "agy", Model: "m1"}) {
		t.Fatal("expected target to be cleared after reset")
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
	s := newServer(func() *config.Config { return cfg }, store, mutate, nil)

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
