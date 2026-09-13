package main

import (
	"bytes"
	"encoding/json"
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
	app.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "openai") {
		t.Fatalf("dashboard: status %d body %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/models", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/models without key: status %d, want 401", rr.Code)
	}
}

func TestHealth(t *testing.T) {
	cfg := &config.Config{}
	store := vault.NewMemoryStore(&vault.Vault{})
	app := newApp(func() *config.Config { return cfg }, store, func(fn func(*config.Config) error) error {
		return fn(cfg)
	}, nil)

	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, httptest.NewRequest("GET", "/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" {
		t.Fatalf("status = %q, want ok", body.Status)
	}
	if body.Version == "" {
		t.Fatal("version should not be empty")
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
