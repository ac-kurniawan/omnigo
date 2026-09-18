package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/quota"
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

	m.RecordProviderRequest(context.Background(), "agy", "gemini-flash", "success")
	m.RecordProviderRequest(context.Background(), "agy", "gemini-flash", "backpressure")
	m.RecordProviderRequest(context.Background(), "agy", "gemini-flash", "upstream_stall")
	m.RecordCombinationAttempt(context.Background(), "auto", "success")
	m.RecordCombinationAttempt(context.Background(), "auto", "failure")
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

// The label must stay bounded whoever supplies the id: an operator-edited
// auth.yaml can hold arbitrary characters, and a rejected request carries no id
// at all. Only validated ids reach the sink, so unknown keys cannot mint
// series, and overlong ids must not collapse distinct keys together.
func TestClientKeyLabelGuard(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty is unattributed", input: "", want: clientKeyNone},
		{name: "generated hex id", input: "a1b2c3d4e5f60718", want: "a1b2c3d4e5f60718"},
		{name: "unsupported chars fall back", input: "key with spaces", want: labelFallback},
		{name: "overlong falls back", input: strings.Repeat("a", 65), want: labelFallback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientKeyLabel(tt.input); got != tt.want {
				t.Fatalf("clientKeyLabel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
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

func TestQuotaMetricLabelSanitization(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "email", input: "user@example.com", want: "user_at_example.com"},
		{name: "uuid account id", input: "fb88ebc3-79ae-45ff-b8b1-2313efa099b4", want: "fb88ebc3-79ae-45ff-b8b1-2313efa099b4"},
		{name: "numeric google subject", input: "105124432875589731973", want: "105124432875589731973"},
		{name: "empty", input: "", want: labelFallback},
		// Overlong is tested separately in TestQuotaLabelBoundedForOverlongIdentity.
		{name: "unsupported chars", input: "weird value!", want: "weird_value_"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quotaAccountLabel(tt.input); got != tt.want {
				t.Fatalf("quotaAccountLabel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestQuotaMetricsExported(t *testing.T) {
	m, err := New(func() bool { return true }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Shutdown(context.Background()) }()

	m.RecordQuotaSample(quota.Sample{
		Provider:       "cx",
		Account:        "fb88ebc3-79ae-45ff-b8b1-2313efa099b4",
		Window:         "primary",
		RemainingRatio: 0.93,
		Status:         1,
		ResetsInSecond: 14134,
	})

	body := scrape(t, m)
	for _, want := range []string{
		"omnigo_provider_quota_remaining_ratio",
		"omnigo_provider_quota_status",
		"omnigo_provider_quota_resets_in_seconds",
		"gen_ai_system=\"cx\"",
		"account=\"fb88ebc3-79ae-45ff-b8b1-2313efa099b4\"",
		"window=\"primary\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("scrape missing %q\n%s", want, body)
		}
	}
}

func TestQuotaEmailAccountLabelNotCollapsedToOther(t *testing.T) {
	m, err := New(func() bool { return true }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Shutdown(context.Background()) }()

	m.RecordQuotaSample(quota.Sample{Provider: "agy", Account: "user@example.com", Window: "gemini-3.8-flash-tiered", RemainingRatio: 0.5, Status: 1})

	body := scrape(t, m)
	if !strings.Contains(body, `account="user_at_example.com"`) {
		t.Fatalf("email account label was collapsed; scrape:\n%s", body)
	}
	if strings.Contains(body, `account="other"`) {
		t.Fatalf("email account collapsed to the fallback label; scrape:\n%s", body)
	}
}

func TestDisabledMetricsRecordsNoQuota(t *testing.T) {
	m := Disabled()
	m.RecordQuotaSample(quota.Sample{Provider: "cx", Account: "a", Window: "primary", RemainingRatio: 1, Status: 1})
	// Disabled() must not panic and must not expose the endpoint; the gauge
	// unavailability is covered by TestDisabledMetricsServes404.
}

// Two distinct logins into one ChatGPT workspace share an AccountID. Labelling
// by AccountID would merge their series, so one account's exhaustion would
// silently overwrite the other's availability. Identity is the unique key.
func TestQuotaLabelKeepsSameAccountIDAccountsDistinct(t *testing.T) {
	first := "fb88ebc3-79ae-45ff-b8b1-2313efa099b4:20110460+networkhr@utc2eduvn.onmicrosoft.com"
	second := "fb88ebc3-79ae-45ff-b8b1-2313efa099b4:user-XTokGp1fOniGFSYxMyTub64g"
	a, b := quotaAccountLabel(first), quotaAccountLabel(second)
	if a == b {
		t.Fatalf("distinct accounts collapsed to the same label %q", a)
	}
	if a == labelFallback || b == labelFallback {
		t.Fatalf("labels fell back to %q (a=%q b=%q)", labelFallback, a, b)
	}
}

func TestQuotaLabelBoundedForOverlongIdentity(t *testing.T) {
	long := strings.Repeat("a", 200) + "@example.com"
	got := quotaAccountLabel(long)
	if got == labelFallback {
		t.Fatalf("overlong identity collapsed to %q instead of a bounded unique label", labelFallback)
	}
	if len(got) > maxLabelLen {
		t.Fatalf("label length %d exceeds max %d", len(got), maxLabelLen)
	}
	if other := quotaAccountLabel(strings.Repeat("a", 200) + "@example.org"); other == got {
		t.Fatal("two overlong identities produced the same label")
	}
}

// An unavailable read has no windows; emitting a ratio for a synthetic window
// would report a fabricated 0% for a window that does not exist.
func TestUnavailableSnapshotEmitsStatusWithoutRatioSeries(t *testing.T) {
	m, err := New(func() bool { return true }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Shutdown(context.Background()) }()

	m.RecordQuotaSample(quota.Sample{Provider: "cx", Account: "acct", Status: -1})

	body := scrape(t, m)
	if !strings.Contains(body, "omnigo_provider_quota_status") {
		t.Fatalf("status series missing:\n%s", body)
	}
	if strings.Contains(body, "omnigo_provider_quota_remaining_ratio{") {
		t.Fatalf("ratio series emitted for a windowless snapshot:\n%s", body)
	}
	if strings.Contains(body, `window="other"`) {
		t.Fatalf("synthetic window label leaked into the scrape:\n%s", body)
	}
}
