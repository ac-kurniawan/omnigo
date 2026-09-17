package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/observability"
	"github.com/ac-kurniawan/omnigo/internal/provider"
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
	}, nil, nil)

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
	app := newApp(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil, nil, nil)
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

func TestPlaygroundDashboardAndV1TelemetryIntegration(t *testing.T) {
	rawKey, hash, prefix, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "openai-test", Type: "openai-test", Models: []string{"gpt-test"}},
		},
		Combos: []config.Combo{
			{Name: "auto-test", Strategy: "priority", Targets: []config.ComboTarget{{Provider: "openai-test", Model: "gpt-test"}}},
		},
	}
	v := &vault.Vault{
		ClientKeys: []vault.ClientKey{
			{ID: "k1", Name: "test-client", KeyHash: hash, Prefix: prefix, Active: true},
		},
	}
	store := vault.NewMemoryStore(v)

	provider.Register("openai-test", func(pcfg provider.Config, cstore provider.CredStore) provider.Provider {
		return &fakeProvider{
			name: pcfg.Name,
			chat: func(r provider.ChatRequest) error {
				return nil
			},
		}
	})

	app := newApp(func() *config.Config { return cfg }, store, nil, nil, nil)

	// 1. Dashboard renders the Inspector & Playground
	dashReq := httptest.NewRequest(http.MethodGet, "/", nil)
	dashReq.SetBasicAuth("admin", "admin")
	dashRec := httptest.NewRecorder()
	app.ServeHTTP(dashRec, dashReq)
	if dashRec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", dashRec.Code)
	}
	dashBody := dashRec.Body.String()
	for _, token := range []string{
		"Gateway Inspector &amp; Playground",
		"playground-key",
		"playground-prompt",
		"playground-send-btn",
		"telemetry-traceid",
		"50,000",
		"auto-test (strategy: priority)",
	} {
		if !strings.Contains(dashBody, token) {
			t.Errorf("dashboard missing playground element %q", token)
		}
	}

	// 2. Client executes request with key against /v1/chat/completions and receives W3C trace & OpenTelemetry headers
	chatBody := `{"model":"auto-test","messages":[{"role":"user","content":"test hello"}]}`
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	chatReq.Header.Set("Authorization", "Bearer "+rawKey)
	chatReq.Header.Set("Content-Type", "application/json")
	inboundTrace := "00-11223344556677889900aabbccddeeff-aabbccddeeff0011-01"
	chatReq.Header.Set("traceparent", inboundTrace)

	chatRec := httptest.NewRecorder()
	app.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body %s", chatRec.Code, chatRec.Body.String())
	}

	respHeaders := chatRec.Result().Header
	traceparent := respHeaders.Get("traceparent")
	if !strings.HasPrefix(traceparent, "00-11223344556677889900aabbccddeeff-") {
		t.Errorf("expected preserved trace ID, got %q", traceparent)
	}
	serverTiming := respHeaders.Get("Server-Timing")
	if !strings.Contains(serverTiming, `gen_ai.system;desc="openai-test"`) {
		t.Errorf("expected gen_ai.system in Server-Timing, got %q", serverTiming)
	}
	if !strings.Contains(serverTiming, `gen_ai.response.model;desc="gpt-test"`) {
		t.Errorf("expected gen_ai.response.model in Server-Timing, got %q", serverTiming)
	}
	if prov := respHeaders.Get("X-OmniGo-Provider"); prov != "openai-test" {
		t.Errorf("expected X-OmniGo-Provider = openai-test, got %q", prov)
	}
	if mod := respHeaders.Get("X-OmniGo-Model"); mod != "gpt-test" {
		t.Errorf("expected X-OmniGo-Model = gpt-test, got %q", mod)
	}
}

func TestActuatorMetricsEndToEndWithApp(t *testing.T) {
	on := true
	cfg := &config.Config{
		Observability: config.Observability{Metrics: &on},
		Providers:     []config.Provider{{Name: "prov-live", Type: "prov-live", Models: []string{"live-m"}}},
	}
	rawKey, hash, prefix, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store := vault.NewMemoryStore(&vault.Vault{
		ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}},
	})
	provider.Register("prov-live", func(pcfg provider.Config, cstore provider.CredStore) provider.Provider {
		return &fakeProvider{name: pcfg.Name}
	})

	metrics, err := observability.New(func() bool {
		return cfg.Observability.MetricsEnabled()
	}, "integration-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = metrics.Shutdown(context.Background()) }()

	app := newApp(func() *config.Config { return cfg }, store, nil, nil, metrics)

	// 1. Initially scrape works and shows the service resource
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("actuator status = %d, want 200", rr.Code)
	}

	// 2. Client executes /v1/chat/completions through the middleware
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"prov-live/live-m","messages":[]}`))
	chatReq.Header.Set("Authorization", "Bearer "+rawKey)
	chatRec := httptest.NewRecorder()
	app.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", chatRec.Code, chatRec.Body.String())
	}

	// 3. Scrape exposes the recorded HTTP duration and provider request count
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		"http_server_request_duration_seconds",
		`http_route="/v1/chat/completions"`,
		"omnigo_provider_requests_total",
		`gen_ai_system="prov-live"`,
		`result="success"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, body)
		}
	}

	// 4. Disabling metrics in config flips the endpoint to 404 without a restart
	off := false
	cfg.Observability.Metrics = &off
	rr = httptest.NewRecorder()
	app.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled status = %d, want 404", rr.Code)
	}
}

type fakeProvider struct {
	name string
	chat func(r provider.ChatRequest) error
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) ChatCompletion(ctx context.Context, r provider.ChatRequest, w http.ResponseWriter) error {
	if f.chat != nil {
		if err := f.chat(r); err != nil {
			return err
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	return nil
}

func (f *fakeProvider) Models(ctx context.Context) ([]provider.Model, error) {
	return []provider.Model{{ID: "gpt-test", Name: "gpt-test"}}, nil
}

func (f *fakeProvider) Test(ctx context.Context) provider.TestResult {
	return provider.TestResult{OK: true, LatencyMS: 10}
}
