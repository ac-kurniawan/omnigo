package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape renders the Prometheus exposition for m, as a scrape would.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

func TestDisabledMetricsServes404(t *testing.T) {
	m := Disabled()
	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled endpoint status = %d, want 404", rr.Code)
	}
}

func TestDisabledMetricsMiddlewarePassesThrough(t *testing.T) {
	m := Disabled()
	called := false
	var gotWriter http.ResponseWriter
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotWriter = w
		w.WriteHeader(http.StatusNoContent)
	})
	rr := httptest.NewRecorder()
	m.Middleware(next).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !called {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if gotWriter != http.ResponseWriter(rr) {
		t.Fatal("disabled middleware must hand the original ResponseWriter to the handler")
	}
}

func TestMetricsRecordsAndExposesHTTPMetrics(t *testing.T) {
	m, err := New(func() bool { return true }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Shutdown(context.Background()) }()

	mux := http.NewServeMux()
	mux.Handle("GET /v1/models", m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})))
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	body := scrape(t, m)
	for _, want := range []string{
		"http_server_request_duration_seconds",
		"http_server_active_requests",
		"http_server_request_time_to_first_byte_seconds",
		`http_route="/v1/models"`,
		`http_request_method="GET"`,
		`http_response_status_code="200"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, body)
		}
	}
}

// The gateway streams SSE by asserting http.Flusher on the ResponseWriter; a
// wrapper that hides Flush would silently buffer every streamed response.
func TestMiddlewarePreservesFlusher(t *testing.T) {
	m, err := New(func() bool { return true }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Shutdown(context.Background()) }()

	var isFlusher, supported bool
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		isFlusher, supported = ok, ok
		if ok {
			flusher.Flush()
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
	if !isFlusher || !supported {
		t.Fatal("instrumented writer must implement http.Flusher")
	}
	if body := scrape(t, m); !strings.Contains(body, "http_server_request_duration_seconds_count") {
		t.Errorf("flushed request not recorded\n%s", body)
	}
}

func TestEnabledGateIsLive(t *testing.T) {
	on := false
	m, err := New(func() bool { return on }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Shutdown(context.Background()) }()

	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled endpoint status = %d, want 404", rr.Code)
	}

	on = true
	rr = httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actuator/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("enabled endpoint status = %d, want 200", rr.Code)
	}
}

func TestRecordProviderOutcomes(t *testing.T) {
	m, err := New(func() bool { return true }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Shutdown(context.Background()) }()

	m.RecordProviderRequest("agy", "gemini-flash", "success")
	m.RecordProviderRequest("agy", "gemini-flash", "backpressure")
	m.RecordProviderRequest("agy", "gemini-flash", "upstream_stall")
	m.RecordCombinationAttempt("auto", "success")
	m.RecordCombinationAttempt("auto", "failure")
	m.RecordConfigReload(true)
	m.RecordConfigReload(false)

	body := scrape(t, m)
	for _, want := range []string{
		"omnigo_provider_requests_total",
		`gen_ai_system="agy"`,
		`result="backpressure"`,
		`result="upstream_stall"`,
		"omnigo_combo_attempts_total",
		`omnigo_combo_name="auto"`,
		"omnigo_config_reloads_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, body)
		}
	}
}

func TestResourceCarriesServiceName(t *testing.T) {
	m, err := New(func() bool { return true }, "v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Shutdown(context.Background()) }()

	body := scrape(t, m)
	if !strings.Contains(body, `service_name="omnigo"`) {
		t.Errorf("scrape missing service_name\n---\n%s", body)
	}
	if !strings.Contains(body, `service_version="v9.9.9"`) {
		t.Errorf("scrape missing service_version\n---\n%s", body)
	}
}
