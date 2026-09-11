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
