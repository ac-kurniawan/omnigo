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
	tr.MarkDrained(combo.Target{Provider: "agy", Model: "m1"}, 1*time.Minute, "upstream 500: internal server error")

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

func TestResetAllDrains(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{{
			Name:     "smart",
			Strategy: "fill-first",
			Targets: []config.ComboTarget{
				{Provider: "agy", Model: "m1"},
				{Provider: "openai", Model: "gpt-4o"},
			},
		}},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	tr := combo.NewTracker("")
	tr.MarkDrained(combo.Target{Provider: "agy", Model: "m1"}, 1*time.Minute, "upstream error")
	tr.MarkDrained(combo.Target{Provider: "openai", Model: "gpt-4o"}, 1*time.Minute, "rate limit")

	s := newServer(func() *config.Config { return cfg }, store, nil, tr)

	if tr.DrainedCount() != 2 {
		t.Fatalf("expected 2 drained targets, got %d", tr.DrainedCount())
	}

	req := httptest.NewRequest("POST", "/combos/drains/reset-all", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if tr.DrainedCount() != 0 {
		t.Fatalf("expected 0 drained targets after reset-all, got %d", tr.DrainedCount())
	}
}

func TestCombosRenderDrainedBadgeWithReason(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{{
			Name:     "smart",
			Strategy: "fill-first",
			Targets:  []config.ComboTarget{{Provider: "openai", Model: "gpt-4o"}},
		}},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	tr := combo.NewTracker("")
	tr.MarkDrained(combo.Target{Provider: "openai", Model: "gpt-4o"}, 2*time.Minute, "rate limited (429)")

	s := newServer(func() *config.Config { return cfg }, store, nil, tr)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/combos", nil)
	req.Header.Set("HX-Request", "true")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "drained") {
		t.Fatalf("expected drained badge in body: %s", body)
	}
	if !strings.Contains(body, "Reason: rate limited (429)") {
		t.Fatalf("expected drain reason in tooltip: %s", body)
	}
}

func TestCombosRenderLiveAlertWhenDrained(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{{
			Name:     "smart",
			Strategy: "fill-first",
			Targets:  []config.ComboTarget{{Provider: "openai", Model: "gpt-4o"}},
		}},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	tr := combo.NewTracker("")

	s := newServer(func() *config.Config { return cfg }, store, nil, tr)

	// Healthy state: operational indicator
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/combos", nil)
	req.Header.Set("HX-Request", "true")
	s.routes().ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "All fallback targets operational") {
		t.Fatalf("expected operational text in body, got: %s", rr.Body.String())
	}

	// Drained state: warning alert banner and Reset All Drains button
	tr.MarkDrained(combo.Target{Provider: "openai", Model: "gpt-4o"}, 1*time.Minute, "rate limit")
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/combos", nil)
	req2.Header.Set("HX-Request", "true")
	s.routes().ServeHTTP(rr2, req2)
	body2 := rr2.Body.String()
	if !strings.Contains(body2, "Reset All Drains") {
		t.Fatalf("expected Reset All Drains button in body, got: %s", body2)
	}
	if !strings.Contains(body2, "1 target in fallback cooldown") {
		t.Fatalf("expected cooldown count in body, got: %s", body2)
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
