// Package observability provides OpenTelemetry-based metrics for the gateway:
// instrumented HTTP handlers, provider/combo outcome counters, and a
// Prometheus exposition endpoint at /actuator/metrics.
//
// Metrics are always constructed so the enabled gate can be flipped by a
// config reload without a restart; when disabled, no recording happens and
// /actuator/metrics answers 404.
package observability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/quota"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	promexp "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	meterName = "omnigo"

	// Scope names for label keys, chosen to match OpenTelemetry semantic
	// conventions where one exists and an omnigo.* namespace otherwise.
	attrProvider = "gen_ai.system"
	attrModel    = "gen_ai.request.model"
	attrResult   = "result"
	attrCombo    = "omnigo.combo.name"

	// attrClientKey carries the id of the gateway client key that authenticated
	// the request, so traffic and outcomes can be grouped per key. The id is
	// the same short identifier the dashboard lists, never the key material or
	// its hash.
	attrClientKey = "api_key_id"

	// Quota label keys. account carries a provider account identifier, never a
	// secret: it is the ChatGPT account id (a UUID) or the Google subject (a
	// numeric id), both already present in the dashboard UI.
	attrAccount = "account"
	attrWindow  = "window"
)

// clientKeyNone is the label value for requests that carry no validated client
// key: dashboard traffic, scrapes, and unauthenticated requests that were
// rejected before a key was ever matched.
const clientKeyNone = "none"

// Metrics holds the meter instruments and the Prometheus scrape endpoint.
type Metrics struct {
	enabled func() bool

	registry *prom.Registry
	provider *sdkmetric.MeterProvider

	requestDuration metric.Float64Histogram
	requestTTFB     metric.Float64Histogram
	activeRequests  metric.Int64UpDownCounter

	providerRequests metric.Int64Counter
	comboAttempts    metric.Int64Counter
	configReloads    metric.Int64Counter

	quotaRemainingRatio metric.Float64Gauge
	quotaStatus         metric.Int64Gauge
	quotaResetsIn       metric.Float64Gauge
}

// New builds the metrics instruments. enabled reports whether recording and
// the scrape endpoint are live; it is consulted per request, so a config
// reload takes effect without a restart.
func New(enabled func() bool, version string) (*Metrics, error) {
	registry := prom.NewRegistry()
	exporter, err := promexp.New(promexp.WithRegisterer(registry))
	if err != nil {
		return nil, err
	}
	res := resource.NewWithAttributes(
		"",
		attribute.String("service.name", "omnigo"),
		attribute.String("service.version", version),
	)
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
	)
	m := &Metrics{enabled: enabled, registry: registry, provider: provider}
	if err := m.init(provider); err != nil {
		_ = provider.Shutdown(context.Background())
		return nil, err
	}
	return m, nil
}

// Disabled returns a Metrics that records nothing and answers 404.
func Disabled() *Metrics {
	m, err := New(func() bool { return false }, "dev")
	if err != nil {
		// The registry is private and freshly built, so registration cannot
		// conflict; a failure here is a programming error.
		panic(err)
	}
	return m
}

func (m *Metrics) init(provider *sdkmetric.MeterProvider) error {
	meter := provider.Meter(meterName)

	httpDurationBuckets := metric.WithExplicitBucketBoundaries(
		0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75,
		1, 2.5, 5, 7.5, 10, 30, 60, 120, 300,
	)
	var err error
	if m.requestDuration, err = meter.Float64Histogram(
		"http.server.request.duration",
		metric.WithDescription("Duration of inbound HTTP requests, from first byte received to response complete"),
		metric.WithUnit("s"),
		httpDurationBuckets,
	); err != nil {
		return err
	}
	if m.requestTTFB, err = meter.Float64Histogram(
		"http.server.request.time_to_first_byte",
		metric.WithDescription("Time from request receipt until the first response byte is written"),
		metric.WithUnit("s"),
		httpDurationBuckets,
	); err != nil {
		return err
	}
	if m.activeRequests, err = meter.Int64UpDownCounter(
		"http.server.active_requests",
		metric.WithDescription("In-flight HTTP requests"),
	); err != nil {
		return err
	}
	if m.providerRequests, err = meter.Int64Counter(
		"omnigo.provider.requests",
		metric.WithDescription("Provider dispatch outcomes"),
	); err != nil {
		return err
	}

	if m.comboAttempts, err = meter.Int64Counter(
		"omnigo.combo.attempts",
		metric.WithDescription("Combo routing attempts and their outcome"),
	); err != nil {
		return err
	}
	if m.configReloads, err = meter.Int64Counter(
		"omnigo.config.reloads",
		metric.WithDescription("Config reload attempts by outcome"),
	); err != nil {
		return err
	}
	if m.quotaRemainingRatio, err = meter.Float64Gauge(
		"omnigo.provider.quota.remaining_ratio",
		metric.WithDescription("Fraction of provider quota still available, per account and window"),
	); err != nil {
		return err
	}
	if m.quotaStatus, err = meter.Int64Gauge(
		"omnigo.provider.quota.status",
		metric.WithDescription("Provider quota status per account: 1 available, 0 exhausted, -1 unavailable"),
	); err != nil {
		return err
	}
	if m.quotaResetsIn, err = meter.Float64Gauge(
		"omnigo.provider.quota.resets_in_seconds",
		metric.WithDescription("Seconds until the provider quota window resets"),
	); err != nil {
		return err
	}
	return nil
}

// Enabled reports whether the live config has metrics on.
func (m *Metrics) Enabled() bool {
	return m != nil && m.enabled != nil && m.enabled()
}

// Handler serves the Prometheus exposition. When metrics are disabled it
// answers 404 so the endpoint is indistinguishable from unregistered.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}
	scrape := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.Enabled() {
			http.NotFound(w, r)
			return
		}
		scrape.ServeHTTP(w, r)
	})
}

// Shutdown flushes and stops the meter provider.
func (m *Metrics) Shutdown(ctx context.Context) error {
	if m == nil || m.provider == nil {
		return nil
	}
	return m.provider.Shutdown(ctx)
}

// Middleware instruments inbound requests with the OpenTelemetry HTTP server
// semantic conventions. It wraps the root handler, so the matched route is only
// known once the inner ServeMux has dispatched: record after next returns.
// When metrics are disabled the next handler receives the original writer.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		method := methodLabel(r.Method)
		start := time.Now()
		activeAttrs := metric.WithAttributes(attribute.String("http.request.method", method))
		m.activeRequests.Add(context.Background(), 1, activeAttrs)
		defer m.activeRequests.Add(context.Background(), -1, activeAttrs)

		caller := &auth.Caller{}
		iw := &instrumentedWriter{ResponseWriter: w, start: start}
		// Rebind r: ServeMux records the matched pattern on the request it
		// dispatches, so routePattern below must read that same request.
		r = r.WithContext(auth.WithCaller(r.Context(), caller))
		next.ServeHTTP(iw, r)

		route := routePattern(r)
		keyID := clientKeyLabel(caller.ID())
		m.requestDuration.Record(context.Background(), time.Since(start).Seconds(), metric.WithAttributes(
			attribute.String("http.route", route),
			attribute.String("http.request.method", method),
			attribute.Int("http.response.status_code", iw.statusCode()),
			attribute.String(attrClientKey, keyID),
		))
		if !iw.firstByte.IsZero() {
			m.requestTTFB.Record(context.Background(), iw.firstByte.Sub(start).Seconds(), metric.WithAttributes(
				attribute.String("http.route", route),
				attribute.String("http.request.method", method),
				attribute.String(attrClientKey, keyID),
			))
		}
	})
}

// routePattern returns the matched path template for r. ServeMux sets Pattern
// to "METHOD /path"; http.route carries the path template alone. Unmatched
// requests collapse into one bucket so a client-chosen path cannot explode
// label cardinality.
func routePattern(r *http.Request) string {
	route := r.Pattern
	if route == "" {
		return "unmatched"
	}
	if i := strings.IndexByte(route, ' '); i >= 0 {
		return route[i+1:]
	}
	return route
}

// instrumentedWriter records the response status and first-byte time while
// forwarding Flush, so SSE passthrough keeps streaming.
type instrumentedWriter struct {
	http.ResponseWriter
	start       time.Time
	firstByte   time.Time
	status      int
	wroteHeader bool
}

func (w *instrumentedWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *instrumentedWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.firstByte.IsZero() {
		w.firstByte = time.Now()
	}
	return w.ResponseWriter.Write(data)
}

// Flush forwards to the underlying writer, preserving streaming behaviour.
func (w *instrumentedWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *instrumentedWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *instrumentedWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// RecordProviderRequest counts a provider dispatch outcome. result is a bounded
// classifier label. provider is a configured name and is still alphabet-checked.
// model is the model actually dispatched — the resolved direct model, or the
// combo target's model after routing — and is recorded as that string. Only an
// empty or overlong value is bounded, so distinct models never share a series.
// The authenticated client key id is read from ctx.
func (m *Metrics) RecordProviderRequest(ctx context.Context, provider, model, result string) {
	if !m.Enabled() {
		return
	}
	m.providerRequests.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String(attrProvider, providerLabel(provider)),
		attribute.String(attrModel, exactModelLabel(model)),
		attribute.String(attrResult, result),
		attribute.String(attrClientKey, clientKeyLabel(auth.CallerFrom(ctx).ID())),
	))
}

// RecordCombinationAttempt counts a combo routing attempt by outcome. The
// authenticated client key id is read from ctx.
func (m *Metrics) RecordCombinationAttempt(ctx context.Context, combo, result string) {
	if !m.Enabled() {
		return
	}
	m.comboAttempts.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String(attrCombo, combo),
		attribute.String(attrResult, result),
		attribute.String(attrClientKey, clientKeyLabel(auth.CallerFrom(ctx).ID())),
	))
}

// Attribute label guards. provider, client key, and method are bounded sets.
// Model ids are recorded exactly: they come from the dispatched request, not
// from a closed alphabet.
const (
	maxLabelLen   = 64
	labelFallback = "other"
)

// safeLabel returns s when it is a short, simple identifier; otherwise it
// collapses to a single fallback value so an attacker-chosen string cannot
// create unbounded series.
func safeLabel(s string) string {
	if s == "" || len(s) > maxLabelLen {
		return labelFallback
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return labelFallback
		}
	}
	return s
}

// clientKeyLabel guards a gateway client key id so an attacker or manual edit
// in auth.yaml cannot create unbounded series. Unauthenticated traffic
// collapses to "none".
func clientKeyLabel(id string) string {
	if id == "" {
		return clientKeyNone
	}
	return safeLabel(id)
}

// providerLabel guards a provider name.
func providerLabel(provider string) string { return safeLabel(provider) }

// exactModelLabel records the dispatched model id. Model ids are not a closed
// alphabet — they contain '/', '+', and other punctuation — so rejecting those
// characters would merge unrelated models into one series. A value longer than
// the label budget keeps a prefix plus a digest of the whole id, same as an
// overlong account, so two long models never share a series.
func exactModelLabel(model string) string {
	if model == "" {
		return labelFallback
	}
	if len(model) <= maxLabelLen {
		return model
	}
	sum := sha256.Sum256([]byte(model))
	suffix := hex.EncodeToString(sum[:4])
	room := maxLabelLen - len(suffix) - 1
	return model[:room] + "-" + suffix
}

// methodLabel guards the HTTP method. Go's server accepts arbitrary method
// tokens, so a client could otherwise mint a series per request.
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions, http.MethodConnect,
		http.MethodTrace:
		return method
	default:
		return labelFallback
	}
}

// quotaAccountLabel guards a provider account identifier.
//
// The label must stay unique per credential. Two logins into one ChatGPT
// workspace share an AccountID, so labelling by AccountID would merge their
// series and let one account's exhaustion overwrite another's availability;
// the credential Identity is therefore the label source.
//
// Values longer than the label budget are truncated with a short digest of the
// whole value rather than collapsed to "other", because collapsing would merge
// every overlong account into one misleading series.
func quotaAccountLabel(account string) string {
	if account == "" {
		return labelFallback
	}
	// Replace @ with _at_ so emails remain readable in Prometheus; all other
	// out-of-alphabet bytes map to _ so the label is always safe for metrics.
	account = strings.ReplaceAll(account, "@", "_at_")
	normalized := make([]byte, len(account))
	for i := range len(account) {
		c := account[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			normalized[i] = c
		case c == '-', c == '_', c == '.', c == ':':
			normalized[i] = c
		default:
			normalized[i] = '_'
		}
	}
	label := string(normalized)
	if len(label) <= maxLabelLen {
		return label
	}
	sum := sha256.Sum256([]byte(account))
	suffix := hex.EncodeToString(sum[:4])
	room := maxLabelLen - len(suffix) - 1
	if room < 0 {
		room = 0
	}
	return label[:room] + "-" + suffix
}

// RecordQuotaSample implements quota.SampleSink. Each gauge records a single
// observation per account/window, so Prometheus holds the latest value rather
// than an ever-growing history.
//
// A sample without a window carries account-level status only: emitting a
// ratio for it would invent a window and report a fabricated 0% remaining.
func (m *Metrics) RecordQuotaSample(sample quota.Sample) {
	if !m.Enabled() {
		return
	}
	base := []attribute.KeyValue{
		attribute.String(attrProvider, providerLabel(sample.Provider)),
		attribute.String(attrAccount, quotaAccountLabel(sample.Account)),
	}
	ctx := context.Background()
	m.quotaStatus.Record(ctx, sample.Status, metric.WithAttributes(base...))
	if sample.Window == "" {
		return
	}
	withWindow := metric.WithAttributes(append(base, attribute.String(attrWindow, exactModelLabel(sample.Window)))...)
	m.quotaRemainingRatio.Record(ctx, sample.RemainingRatio, withWindow)
	m.quotaResetsIn.Record(ctx, sample.ResetsInSecond, withWindow)
}

// RecordConfigReload counts a config reload by outcome.
func (m *Metrics) RecordConfigReload(ok bool) {
	if !m.Enabled() {
		return
	}
	m.configReloads.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String(attrResult, strconv.FormatBool(ok)),
	))
}
