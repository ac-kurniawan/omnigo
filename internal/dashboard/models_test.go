package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestDashboardManualAddModel(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}},
		},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		return fn(cfg)
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate)

	req := httptest.NewRequest("POST", "/providers/openai/models", strings.NewReader("model_id=gpt-4.5-preview"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(cfg.Providers[0].Models) != 2 || cfg.Providers[0].Models[1] != "gpt-4.5-preview" {
		t.Fatalf("models = %v", cfg.Providers[0].Models)
	}
}

func TestDisabledModelRowIncludesCopyIDAction(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "openai", Type: "openai", DisabledModels: []string{"gpt-3.5-turbo"}},
		},
	}
	h := testHandler(t, cfg, &vault.Vault{})
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	want := `onclick="copyToClipboard('gpt-3.5-turbo', this, 'Copied!')">Copy ID</button>`
	if !strings.Contains(rr.Body.String(), want) {
		t.Fatalf("disabled model row missing copy action %q, body = %s", want, rr.Body.String())
	}
}

func TestDashboardDisableAndEnableModel(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "openai", Type: "openai", Models: []string{"gpt-4o", "gpt-4o-mini"}},
		},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		return fn(cfg)
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate)

	// Disable gpt-4o
	req := httptest.NewRequest("POST", "/providers/openai/models/disable", strings.NewReader("model_id=gpt-4o"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	p := cfg.Providers[0]
	if len(p.Models) != 1 || p.Models[0] != "gpt-4o-mini" {
		t.Fatalf("active models = %v", p.Models)
	}
	if len(p.DisabledModels) != 1 || p.DisabledModels[0] != "gpt-4o" {
		t.Fatalf("disabled models = %v", p.DisabledModels)
	}

	// Enable gpt-4o
	req = httptest.NewRequest("POST", "/providers/openai/models/enable", strings.NewReader("model_id=gpt-4o"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr = httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	p = cfg.Providers[0]
	if len(p.Models) != 2 {
		t.Fatalf("expected 2 active models, got %v", p.Models)
	}
	if len(p.DisabledModels) != 0 {
		t.Fatalf("expected 0 disabled models, got %v", p.DisabledModels)
	}
}

func TestDashboardDeleteModel(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}, DisabledModels: []string{"gpt-3.5-turbo"}},
		},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		return fn(cfg)
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate)

	req := httptest.NewRequest("POST", "/providers/openai/models/delete", strings.NewReader("model_id=gpt-4o"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	p := cfg.Providers[0]
	if len(p.Models) != 0 {
		t.Fatalf("expected 0 models, got %v", p.Models)
	}
}

func TestDashboardBulkDisableAndEnableModels(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "openai", Type: "openai", Models: []string{"m1", "m2", "m3"}},
		},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		return fn(cfg)
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate)

	// Bulk disable m1 and m2
	req := httptest.NewRequest("POST", "/providers/openai/models/disable", strings.NewReader("model_id=m1&model_id=m2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	p := cfg.Providers[0]
	if len(p.Models) != 1 || p.Models[0] != "m3" {
		t.Fatalf("expected only m3 active, got %v", p.Models)
	}
	if len(p.DisabledModels) != 2 {
		t.Fatalf("expected 2 disabled models, got %v", p.DisabledModels)
	}
	// Verify response targets modal content and OOB chips, not full providers table
	body := rr.Body.String()
	if !strings.Contains(body, "models-content-openai") {
		t.Fatalf("body should contain models-content-openai to keep dialog open, got: %s", body)
	}
	if !strings.Contains(body, "model-chips-openai") {
		t.Fatalf("body should contain model-chips-openai OOB swap, got: %s", body)
	}
	if strings.Contains(body, "<table class=\"table w-full\">") {
		t.Fatalf("body should not re-render full table (which unmounts dialog): %s", body)
	}

	// Bulk enable m1 and m2 back
	req = httptest.NewRequest("POST", "/providers/openai/models/enable", strings.NewReader("model_id=m1&model_id=m2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr = httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	p = cfg.Providers[0]
	if len(p.Models) != 3 {
		t.Fatalf("expected 3 active models, got %v", p.Models)
	}
	if len(p.DisabledModels) != 0 {
		t.Fatalf("expected 0 disabled models, got %v", p.DisabledModels)
	}
}
