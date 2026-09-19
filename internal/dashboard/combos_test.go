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

func TestAddComboAcceptsReliableAndRoundRobin(t *testing.T) {
	for _, strategy := range []string{"reliable", "round-robin"} {
		t.Run(strategy, func(t *testing.T) {
			cfg := &config.Config{Providers: []config.Provider{{Name: "openai", Type: "openai"}}}
			store := vault.NewMemoryStore(&vault.Vault{})
			mutate := func(fn func(*config.Config) error) error {
				if err := fn(cfg); err != nil {
					return err
				}
				return cfg.Validate()
			}
			s := newServer(func() *config.Config { return cfg }, store, mutate, nil)
			req := httptest.NewRequest("POST", "/combos", strings.NewReader("name=smart&strategy="+strategy+"&targets=openai/gpt-4o"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("HX-Request", "true")
			rr := httptest.NewRecorder()
			s.routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK || len(cfg.Combos) != 1 || cfg.Combos[0].Strategy != strategy {
				t.Fatalf("status = %d, combos = %+v", rr.Code, cfg.Combos)
			}
		})
	}
}

func TestAddComboRejectsUnknownStrategy(t *testing.T) {
	cfg := &config.Config{}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		if err := fn(cfg); err != nil {
			return err
		}
		return cfg.Validate()
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate, nil)
	req := httptest.NewRequest("POST", "/combos", strings.NewReader("name=smart&strategy=random&targets=openai/gpt-4o"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
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

func newComboTestServer(t *testing.T, cfg *config.Config, tracker *combo.Tracker) *Server {
	t.Helper()
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		if err := fn(cfg); err != nil {
			return err
		}
		return cfg.Validate()
	}
	return newServer(func() *config.Config { return cfg }, store, mutate, tracker)
}

func TestUpdateComboFullReplace(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{{
			Name:     "smart",
			Strategy: "priority",
			Targets: []config.ComboTarget{
				{Provider: "agy", Model: "gemini-3.7-flash"},
				{Provider: "openai-main", Model: "gpt-4o"},
			},
			DrainTTL: "90s",
		}},
	}
	s := newComboTestServer(t, cfg, nil)

	body := "strategy=round-robin&targets=openai-main/gpt-4o-mini,agy/gemini-3.7-pro"
	req := httptest.NewRequest("POST", "/combos/smart", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got := cfg.Combos[0]
	if got.Strategy != "round-robin" {
		t.Fatalf("strategy = %q, want round-robin", got.Strategy)
	}
	want := []config.ComboTarget{
		{Provider: "openai-main", Model: "gpt-4o-mini"},
		{Provider: "agy", Model: "gemini-3.7-pro"},
	}
	if len(got.Targets) != len(want) {
		t.Fatalf("targets = %+v, want %+v", got.Targets, want)
	}
	for i := range want {
		if got.Targets[i] != want[i] {
			t.Fatalf("targets[%d] = %+v, want %+v", i, got.Targets[i], want[i])
		}
	}
	if got.DrainTTL != "90s" {
		t.Fatalf("drain_ttl = %q, want preserved 90s", got.DrainTTL)
	}
}

func TestUpdateComboRejectsEmptyTargets(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{{
			Name:     "smart",
			Strategy: "priority",
			Targets:  []config.ComboTarget{{Provider: "agy", Model: "gemini-3.7-flash"}},
		}},
	}
	s := newComboTestServer(t, cfg, nil)

	req := httptest.NewRequest("POST", "/combos/smart", strings.NewReader("strategy=priority&targets="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if len(cfg.Combos[0].Targets) != 1 || cfg.Combos[0].Targets[0].Model != "gemini-3.7-flash" {
		t.Fatalf("combo mutated on rejected update: %+v", cfg.Combos[0])
	}
}

func TestUpdateComboRejectsUnknownStrategy(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{{
			Name:     "smart",
			Strategy: "priority",
			Targets:  []config.ComboTarget{{Provider: "agy", Model: "gemini-3.7-flash"}},
		}},
	}
	s := newComboTestServer(t, cfg, nil)

	req := httptest.NewRequest("POST", "/combos/smart", strings.NewReader("strategy=weighted&targets=agy/gemini-3.7-flash"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if cfg.Combos[0].Strategy != "priority" {
		t.Fatalf("strategy mutated to %q on rejected update", cfg.Combos[0].Strategy)
	}
}

func TestUpdateComboUnknownCombo(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{{Name: "smart", Strategy: "priority", Targets: []config.ComboTarget{{Provider: "agy", Model: "m"}}}},
	}
	s := newComboTestServer(t, cfg, nil)

	req := httptest.NewRequest("POST", "/combos/nope", strings.NewReader("strategy=priority&targets=agy/m"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateComboResetsCooldownsForNewChain(t *testing.T) {
	drained := combo.Target{Provider: "openai-main", Model: "gpt-4o"}
	untouched := combo.Target{Provider: "other", Model: "m1"}
	cfg := &config.Config{
		Combos: []config.Combo{{
			Name:     "smart",
			Strategy: "priority",
			Targets:  []config.ComboTarget{{Provider: "agy", Model: "gemini-3.7-flash"}},
		}},
	}
	tracker := combo.NewTracker("")
	tracker.MarkDrained(drained, time.Minute, "quota")
	tracker.MarkDrained(untouched, time.Minute, "quota")
	s := newComboTestServer(t, cfg, tracker)

	// New chain re-includes the drained target and drops nothing else.
	body := "strategy=priority&targets=openai-main/gpt-4o"
	req := httptest.NewRequest("POST", "/combos/smart", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if tracker.IsDrained(drained) {
		t.Fatal("target in the new chain is still drained after update")
	}
	if !tracker.IsDrained(untouched) {
		t.Fatal("cooldown of an unrelated target was cleared by the update")
	}
}

func TestCombosRenderEditButtonAndHiddenTargets(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{{
			Name:     "smart",
			Strategy: "priority",
			Targets: []config.ComboTarget{
				{Provider: "agy", Model: "gemini-3.7-flash"},
				{Provider: "openai-main", Model: "gpt-4o"},
			},
		}},
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
	wantHidden := `<input type="hidden" name="targets" value="agy/gemini-3.7-flash,openai-main/gpt-4o">`
	if !strings.Contains(body, wantHidden) {
		t.Fatalf("rendered table missing hidden targets input: %s", body)
	}
	wantEditBtn := `openEditComboModal('smart', 'priority', 'agy/gemini-3.7-flash,openai-main/gpt-4o')`
	if !strings.Contains(body, wantEditBtn) {
		t.Fatalf("rendered table missing openEditComboModal call: %s", body)
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

func TestComboPickerModelsReflectRefreshedCatalog(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.7-flash"}}},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	s := newServer(func() *config.Config { return cfg }, store, nil, nil)

	// Simulates the provider Refresh button updating the cached model list
	// after the dashboard page was first rendered.
	cfg.Providers[0].Models = append(cfg.Providers[0].Models, "gemini-3.8-ultra")

	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest("GET", "/combo-picker/models", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-target="agy/gemini-3.8-ultra"`) {
		t.Fatalf("picker missing refreshed model: %s", body)
	}
}
