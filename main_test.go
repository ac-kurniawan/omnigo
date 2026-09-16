package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestAppServesDashboardAndGateV1(t *testing.T) {
	cfg := &config.Config{
		Server:    config.Server{Host: "127.0.0.1", Port: 8080},
		Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{"gpt-4o"}}},
		Combos:    []config.Combo{{Name: "auto", Strategy: "priority"}},
	}
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	app := newApp(func() *config.Config { return cfg }, store, func(fn func(*config.Config) error) error {
		return fn(cfg)
	}, nil)

	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, httptest.NewRequest("GET", "/health", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"ok"`) || !strings.Contains(rr.Body.String(), `"version":"dev"`) {
		t.Fatalf("health: status %d body %s", rr.Code, rr.Body.String())
	}

	// Unauthenticated request to dashboard is rejected by default (401)
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("dashboard without auth: status %d, want 401", rr.Code)
	}

	// Authenticated request to dashboard with default credentials succeeds (200)
	rr = httptest.NewRecorder()
	dashReq := httptest.NewRequest("GET", "/", nil)
	dashReq.SetBasicAuth("admin", "admin")
	app.ServeHTTP(rr, dashReq)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "openai") {
		t.Fatalf("dashboard with default auth: status %d body %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/models", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/models without key: status %d, want 401", rr.Code)
	}
}

func TestAppKeepsV1ProtectedFromDashboardCredentials(t *testing.T) {
	cfg := &config.Config{}
	app := newApp(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "omnigo.test"
	req.Header.Set("Origin", "http://omnigo.test")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-Omnigo-CSRF", "dashboard-token")
	req.AddCookie(&http.Cookie{Name: "omnigo_csrf", Value: "dashboard-token"})
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), `"code":"invalid_api_key"`) {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
}

func TestReloadSwapsConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	authPath := filepath.Join(dir, "auth.yaml")
	key := bytes.Repeat([]byte{0xAB}, 32)

	if err := os.WriteFile(cfgPath, []byte("server:\n  port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := newState(cfgPath, authPath, key)
	if err != nil {
		t.Fatal(err)
	}
	if s.getCfg().Server.Port != 8080 {
		t.Fatalf("port = %d, want 8080", s.getCfg().Server.Port)
	}

	if err := os.WriteFile(cfgPath, []byte("server:\n  port: 9090\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if s.getCfg().Server.Port != 9090 {
		t.Fatalf("port = %d, want 9090 after reload", s.getCfg().Server.Port)
	}
}

func TestVersionDefault(t *testing.T) {
	if version == "" {
		t.Fatal("version should not be empty")
	}
}
