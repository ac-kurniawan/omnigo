package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/observability"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// newEndpointTestRouter builds a router backed by real metrics whose gate reads
// the supplied live config, mirroring main's wiring.
func newEndpointTestRouter(t *testing.T, cfg *config.Config) (http.Handler, *observability.Metrics) {
	t.Helper()
	metrics, err := observability.New(func() bool { return cfg.Observability.MetricsEnabled() }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })
	return NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil, nil, "test-version", metrics), metrics
}

func TestActuatorMetricsDisabledByDefault(t *testing.T) {
	router, _ := newEndpointTestRouter(t, &config.Config{})
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when metrics disabled by default", rr.Code)
	}
}

func TestActuatorMetricsEnabledServesPrometheus(t *testing.T) {
	on := true
	router, _ := newEndpointTestRouter(t, &config.Config{Observability: config.Observability{Metrics: &on}})
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when enabled (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "# TYPE") {
		t.Fatalf("expected Prometheus exposition, got %s", rr.Body.String())
	}
}

// A config reload must flip the endpoint without a restart: the gate is read
// from the live config snapshot on each request.
func TestActuatorMetricsFollowsConfigReload(t *testing.T) {
	off, on := false, true
	cfg := &config.Config{Observability: config.Observability{Metrics: &off}}
	router, _ := newEndpointTestRouter(t, cfg)

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 while disabled", rr.Code)
	}

	cfg.Observability.Metrics = &on
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after enabling", rr.Code)
	}

	cfg.Observability.Metrics = &off
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 after disabling", rr.Code)
	}
}

// testMetrics records dispatch outcomes for assertions.
type testMetrics struct {
	providerCalls [][3]string
	comboCalls    [][2]string
}

func (m *testMetrics) RecordProviderRequest(provider, model, result string) {
	m.providerCalls = append(m.providerCalls, [3]string{provider, model, result})
}

func (m *testMetrics) RecordCombinationAttempt(combo, result string) {
	m.comboCalls = append(m.comboCalls, [2]string{combo, result})
}

// newRecordingRouter builds an authenticated router that records outcomes.
func newRecordingRouter(t *testing.T, cfg *config.Config, m Metrics) (http.Handler, string) {
	t.Helper()
	raw, hash, prefix, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	return NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, nil, "test-version", m), raw
}

func TestComboRecordsProviderAndComboOutcomes(t *testing.T) {
	provider.Register("openai", func(pcfg provider.Config, store provider.CredStore) provider.Provider {
		if pcfg.Name == "bad" {
			return &fakeProvider{name: pcfg.Name, chat: func(r provider.ChatRequest) error {
				return provider.NewHTTPStatusError(http.StatusInternalServerError, "upstream down")
			}}
		}
		return &fakeProvider{name: pcfg.Name, chat: func(r provider.ChatRequest) error { return nil }}
	})

	m := &testMetrics{}
	router, raw := newRecordingRouter(t, reliableTestConfig(), m)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"safe","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 via fallback (body %s)", rr.Code, rr.Body.String())
	}

	var sawBad, sawGood bool
	for _, call := range m.providerCalls {
		if call[0] == "bad" && call[2] == resultUpstreamError {
			sawBad = true
		}
		if call[0] == "good" && call[2] == resultSuccess {
			sawGood = true
		}
	}
	if !sawBad {
		t.Errorf("expected bad provider upstream_error outcome, got %v", m.providerCalls)
	}
	if !sawGood {
		t.Errorf("expected good provider success outcome, got %v", m.providerCalls)
	}
	if len(m.comboCalls) != 1 || m.comboCalls[0][0] != "safe" || m.comboCalls[0][1] != resultSuccess {
		t.Errorf("expected one successful combo attempt for \"safe\", got %v", m.comboCalls)
	}
}

func TestDirectPathRecordsProviderOutcome(t *testing.T) {
	provider.Register("openai", func(pcfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: pcfg.Name, chat: func(r provider.ChatRequest) error { return nil }}
	})
	cfg := &config.Config{Providers: []config.Provider{{Name: "solo", Type: "openai", Models: []string{"m1"}}}}
	m := &testMetrics{}
	router, raw := newRecordingRouter(t, cfg, m)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"solo/m1","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	router.ServeHTTP(httptest.NewRecorder(), req)

	if len(m.providerCalls) != 1 || m.providerCalls[0] != [3]string{"solo", "m1", resultSuccess} {
		t.Fatalf("expected solo/m1 success, got %v", m.providerCalls)
	}
}

func TestClassifyProviderError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		committed bool
		client    bool
		want      string
	}{
		{name: "nil is success", err: nil, want: resultSuccess},
		{name: "stall", err: provider.ErrUpstreamStall, want: resultUpstreamStall},
		{name: "wrapped stall", err: wrappedErr{provider.ErrUpstreamStall}, want: resultUpstreamStall},
		{name: "backpressure", err: &provider.ProviderBusyError{Limit: 2}, want: resultBackpressure},
		{name: "upstream 429", err: provider.NewHTTPStatusError(http.StatusTooManyRequests, ""), want: resultRateLimited},
		{name: "upstream 500", err: provider.NewHTTPStatusError(http.StatusInternalServerError, ""), want: resultUpstreamError},
		{name: "committed failure", err: errResponseCommitted, committed: true, want: resultStreamFailed},
		{name: "client abort wins", err: context.Canceled, committed: true, client: true, want: resultClientAbort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyProviderError(tt.err, tt.committed, tt.client); got != tt.want {
				t.Fatalf("classifyProviderError = %q, want %q", got, tt.want)
			}
		})
	}
}

type wrappedErr struct{ err error }

func (w wrappedErr) Error() string { return "wrapped: " + w.err.Error() }
func (w wrappedErr) Unwrap() error { return w.err }

// Metric labels must never be client-controlled: an attacker who can mint a
// distinct label value per request can grow the series set without bound.
// These two paths are reachable by anyone holding an API key.
func TestModelLabelIsBoundedToConfiguredCatalog(t *testing.T) {
	provider.Register("probe", func(pcfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: pcfg.Name, chat: func(r provider.ChatRequest) error {
			return provider.NewHTTPStatusError(http.StatusInternalServerError, "nope")
		}}
	})
	on := true
	cfg := &config.Config{
		Observability: config.Observability{Metrics: &on},
		Providers:     []config.Provider{{Name: "probe", Type: "probe", Models: []string{"real-model"}}},
	}
	metrics, err := observability.New(func() bool { return true }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = metrics.Shutdown(context.Background()) }()

	raw, hash, prefix, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	router := NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, nil, "test-version", metrics)

	// Each of these is accepted by the provider resolver but is not in the
	// provider's configured catalog.
	for _, suffix := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "b1"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"probe/ATTACK-`+suffix+`","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+raw)
		router.ServeHTTP(httptest.NewRecorder(), req)
	}

	rr := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	body := rr.Body.String()
	if strings.Contains(body, "ATTACK-") {
		t.Fatalf("client-controlled model became a metric label\n%s", body)
	}
	if n := strings.Count(body, `gen_ai_request_model="`); n > 2 {
		t.Fatalf("model label cardinality = %d, want the configured catalog size\n%s", n, body)
	}
}

func TestMethodLabelIsBoundedToKnownMethods(t *testing.T) {

	metrics, err := observability.New(func() bool { return true }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = metrics.Shutdown(context.Background()) }()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	for _, method := range []string{"FOOBAR-1", "FOOBAR-2", "FOOBAR-3"} {
		rr := httptest.NewRecorder()
		metrics.Middleware(inner).ServeHTTP(rr, httptest.NewRequest(method, "/health", nil))
	}

	rr := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	body := rr.Body.String()
	if strings.Contains(body, "FOOBAR") {
		t.Fatalf("client-controlled method became a metric label\n%s", body)
	}
	if !strings.Contains(body, `http_request_method="other"`) {
		t.Fatalf("expected unknown methods to collapse to \"other\"\n%s", body)
	}
}
