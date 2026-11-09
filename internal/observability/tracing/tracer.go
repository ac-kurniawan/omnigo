// Package tracing exports one OpenTelemetry generation span per upstream
// dispatch to a Langfuse project over OTLP/HTTP, plus an inbound request span
// that parents them.
//
// Sampling and the exporter both read live state through Options, so a config
// reload flips export on or off without a restart.
package tracing

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "omnigo"

	// attrUserID and attrTraceName are Langfuse's native OTel attributes.
	attrUserID    = "langfuse.user.id"
	attrTraceName = "langfuse.trace.name"
)

// Keys is a Langfuse project key pair.
type Keys struct {
	PublicKey string
	SecretKey string
}

// Options wires the tracer to live state. Every func is consulted per span
// start or per export, so a config reload or a key save takes effect without
// a restart.
type Options struct {
	ServiceVersion string
	Enabled        func() bool
	Host           func() string
	Keys           func() Keys
	Capture        func() bool
}

// Tracer owns the span provider and rebuilds the exporter when the endpoint or
// the key pair changes.
type Tracer struct {
	opts     Options
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer

	mu      sync.Mutex
	exp     sdktrace.SpanExporter
	expKeys exporterKey
}

type exporterKey struct {
	host      string
	publicKey string
	secretKey string
}

// New builds the tracer. Export happens only while Enabled reports true: the
// sampler drops every span otherwise, so a disabled gateway pays no export
// cost and a reload turns export on without a restart.
func New(opts Options) (*Tracer, error) {
	t := &Tracer{opts: opts}
	// Build one exporter up front so the first span has somewhere to go.
	if host := hostOf(opts); host != "" {
		exp, key, err := t.newExporter(host, opts.Keys())
		if err != nil {
			return nil, err
		}
		t.exp, t.expKeys = exp, key
		t.provider = sdktrace.NewTracerProvider(
			sdktrace.WithSampler(liveSampler{enabled: opts.Enabled}),
			sdktrace.WithResource(resourceOf(opts)),
			sdktrace.WithBatcher(exp),
		)
	} else {
		t.provider = sdktrace.NewTracerProvider(
			sdktrace.WithSampler(liveSampler{enabled: opts.Enabled}),
			sdktrace.WithResource(resourceOf(opts)),
		)
	}
	t.tracer = t.provider.Tracer(tracerName)
	return t, nil
}

// newWithProcessor is the test seam: spans go to proc as they end.
func newWithProcessor(opts Options, proc sdktrace.SpanProcessor) *Tracer {
	t := &Tracer{opts: opts}
	t.provider = sdktrace.NewTracerProvider(
		sdktrace.WithSampler(liveSampler{enabled: opts.Enabled}),
		sdktrace.WithSpanProcessor(proc),
	)
	t.tracer = t.provider.Tracer(tracerName)
	return t
}

func hostOf(opts Options) string {
	if opts.Host == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(opts.Host()), "/")
}

func resourceOf(opts Options) *resource.Resource {
	return resource.NewWithAttributes(
		"",
		attribute.String("service.name", "omnigo"),
		attribute.String("service.version", opts.ServiceVersion),
	)
}

func (t *Tracer) newExporter(host string, keys Keys) (sdktrace.SpanExporter, exporterKey, error) {
	endpoint := strings.TrimRight(host, "/") + "/api/public/otel/v1/traces"
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}
	if keys.PublicKey != "" || keys.SecretKey != "" {
		basic := base64.StdEncoding.EncodeToString([]byte(keys.PublicKey + ":" + keys.SecretKey))
		opts = append(opts, otlptracehttp.WithHeaders(map[string]string{
			"Authorization":                "Basic " + basic,
			"x-langfuse-ingestion-version": "4",
		}))
	}
	exp, err := otlptracehttp.New(context.Background(), opts...)
	if err != nil {
		return nil, exporterKey{}, fmt.Errorf("langfuse exporter: %w", err)
	}
	return exp, exporterKey{host: host, publicKey: keys.PublicKey, secretKey: keys.SecretKey}, nil
}

// ensureExporter rebuilds the exporter when the endpoint or the key pair
// changed since the current one was built, and closes the old one.
func (t *Tracer) ensureExporter() {
	if t.opts.Keys == nil {
		return
	}
	host := hostOf(t.opts)
	keys := t.opts.Keys()
	want := exporterKey{host: host, publicKey: keys.PublicKey, secretKey: keys.SecretKey}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exp != nil && t.expKeys == want {
		return
	}
	exp, key, err := t.newExporter(host, keys)
	if err != nil {
		// Keep the previous exporter: dropping spans beats failing a request
		// over an endpoint the config loader already validated.
		return
	}
	old := t.exp
	t.exp, t.expKeys = exp, key
	t.provider.RegisterSpanProcessor(sdktrace.NewBatchSpanProcessor(exp))
	if old != nil {
		_ = old.Shutdown(context.Background())
	}
}

// Shutdown flushes and stops the provider.
func (t *Tracer) Shutdown(ctx context.Context) error {
	if t == nil || t.provider == nil {
		return nil
	}
	t.mu.Lock()
	t.exp = nil
	t.mu.Unlock()
	return t.provider.Shutdown(ctx)
}

// excludedRoutes are dashboard and scrape paths that produce no Langfuse
// trace: polling UI fragments and metrics scrapes would only add noise.
// "/" covers the dashboard index route, declared as "GET /{$}".
var excludedRoutes = map[string]bool{
	"/":                  true,
	"/actuator/metrics":  true,
	"/combos":            true,
	"/playground/models": true,
	"/settings":          true,
}

// Middleware starts the inbound request span and fills in the caller identity,
// route, and status once the handler returns. The identity is only known after
// auth runs inside the mux, which is why these attributes are set last.
// Excluded routes produce no span: they are matched on the request path before
// dispatch, because ServeMux fills in r.Pattern only further down the chain.
func (t *Tracer) Middleware(next http.Handler) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t.opts.Enabled != nil && !t.opts.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		if excludedRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		t.ensureExporter()
		ctx := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := t.tracer.Start(ctx, "http.request", trace.WithSpanKind(trace.SpanKindServer))
		r = r.WithContext(ctx)

		ww := &spanWriter{ResponseWriter: w}
		next.ServeHTTP(ww, r)

		route := routePattern(r)
		span.SetAttributes(
			attribute.String("http.route", route),
			attribute.Int("http.response.status_code", ww.statusCode()),
			attribute.String(attrTraceName, route),
		)
		if id := auth.CallerFrom(ctx).ID(); id != "" {
			span.SetAttributes(attribute.String(attrUserID, id))
		}
		span.End()
	})
}

// excludedRoute reports whether path belongs to a route that must not reach
// Langfuse. /static/ is matched as a prefix; every other entry is exact.
func excludedRoute(path string) bool {
	return excludedRoutes[path] || strings.HasPrefix(path, "/static/")
}

// routePattern returns the matched path template, collapsing unmatched
// requests into one bucket (ServeMux sets Pattern to "METHOD /path").
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

// spanWriter records the status while forwarding Flush, so SSE keeps
// streaming.
type spanWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *spanWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *spanWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *spanWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *spanWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *spanWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// liveSampler samples only while enabled and delegates to the default
// parent-based sampler then. Sampling is decided per span start, so a reload
// flips it immediately.
type liveSampler struct{ enabled func() bool }

func (s liveSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if s.enabled == nil || s.enabled() {
		return sdktrace.ParentBased(sdktrace.AlwaysSample()).ShouldSample(p)
	}
	return sdktrace.SamplingResult{Decision: sdktrace.Drop}
}

func (s liveSampler) Description() string { return "omnigo-live" }
