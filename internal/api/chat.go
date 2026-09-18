package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func handleChat(getCfg func() *config.Config, registry *providerRegistry, tracker *combo.Tracker, m Metrics) http.HandlerFunc {
	metrics := orNoop(m)
	return func(w http.ResponseWriter, r *http.Request) {
		startTime := time.Now()
		tc := NewTraceContext(r.Header.Get("traceparent"))
		r = r.WithContext(WithTraceContext(r.Context(), tc))
		w.Header().Set("traceparent", tc.Traceparent())

		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		var body struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.Model == "" {
			writeError(w, http.StatusBadRequest, "model is required")
			return
		}
		cfg := getCfg()

		msgs := make([]provider.Message, 0, len(body.Messages))
		for _, m := range body.Messages {
			msgs = append(msgs, provider.Message{Role: m.Role, Content: m.Content})
		}
		req := provider.ChatRequest{Model: body.Model, Stream: body.Stream, Messages: msgs, Raw: raw}

		if cb, ok := findCombo(cfg, body.Model); ok {
			runCombo(w, r, cfg, registry, cb, req, tracker, tc, startTime, metrics)
			return
		}

		p, provName, model, ok := resolveDirect(cfg, registry, body.Model)
		if !ok {
			writeError(w, http.StatusNotFound, "model not found: "+body.Model)
			return
		}
		req.Model = model
		SetTelemetryHeaders(w, tc, provName, model, startTime)
		// Providers stream straight to the client on this path, so a late failure
		// must not be answered with a fresh JSON error envelope: the SSE body is
		// already on the wire and the object would be parsed as a bad frame.
		tracked := &commitTracker{ResponseWriter: w}
		providerErr := p.ChatCompletion(r.Context(), req, tracked)
		metrics.RecordProviderRequest(r.Context(), provName, knownModelLabel(cfg, provName, model), classifyProviderError(providerErr, tracked.committed, r.Context().Err() != nil))
		if providerErr != nil && !tracked.committed {
			writeProviderError(w, providerErr)
		}
	}
}

func findCombo(cfg *config.Config, name string) (config.Combo, bool) {
	for _, cb := range cfg.Combos {
		if cb.Name == name {
			return cb, true
		}
	}
	return config.Combo{}, false
}

func resolveDirect(cfg *config.Config, registry *providerRegistry, model string) (provider.Provider, string, string, bool) {
	if i := strings.Index(model, "/"); i > 0 {
		provName := model[:i]
		modelID := model[i+1:]
		for _, pc := range cfg.Providers {
			if pc.Name == provName {
				if pc.Disabled || pc.IsModelDisabled(modelID) {
					return nil, "", "", false
				}
				p, ok := registry.Get(cfg, pc.Name)
				if !ok {
					return nil, "", "", false
				}
				return p, pc.Name, modelID, true
			}
		}
	}
	for _, pc := range cfg.Providers {
		if pc.Disabled {
			continue
		}
		for _, m := range pc.Models {
			if m == model {
				if pc.IsModelDisabled(model) {
					return nil, "", "", false
				}
				p, ok := registry.Get(cfg, pc.Name)
				if !ok {
					return nil, "", "", false
				}
				return p, pc.Name, model, true
			}
		}
	}
	return nil, "", "", false
}

func runCombo(w http.ResponseWriter, r *http.Request, cfg *config.Config, registry *providerRegistry, cb config.Combo, req provider.ChatRequest, tracker *combo.Tracker, tc *TraceContext, startTime time.Time, metrics Metrics) {
	c := combo.Combo{
		Name:     cb.Name,
		Strategy: cb.Strategy,
		Tracker:  tracker,
		DrainTTL: cb.ParsedDrainTTL(),
	}
	for _, t := range cb.Targets {
		c.Targets = append(c.Targets, combo.Target{Provider: t.Provider, Model: t.Model})
	}
	responseCommitted := false
	_, err := c.Run(r.Context(), func(ctx context.Context, t combo.Target) error {
		p, ok := providerForName(cfg, registry, t.Provider)
		if !ok {
			metrics.RecordProviderRequest(ctx, t.Provider, t.Model, resultUnavailable)
			return fmt.Errorf("unknown provider: %s", t.Provider)
		}
		for _, pc := range cfg.Providers {
			if pc.Name == t.Provider && pc.IsModelDisabled(t.Model) {
				metrics.RecordProviderRequest(ctx, t.Provider, t.Model, resultUnavailable)
				return fmt.Errorf("model %q is disabled on provider %q", t.Model, t.Provider)
			}
		}
		req.Model = t.Model
		attempt := newBufferedResponseWriter(w, req.Stream)
		SetTelemetryHeaders(attempt, tc, t.Provider, t.Model, startTime)
		providerErr := p.ChatCompletion(ctx, req, attempt)
		err := attempt.finish(providerErr)
		if errors.Is(err, errResponseCommitted) {
			responseCommitted = true
			// A client abort mid-stream cancels the request context; that is not an
			// upstream fault, so the target stays healthy. A stalled upstream leaves
			// the request context alive and is still drained.
			clientGone := r.Context().Err() != nil
			metrics.RecordProviderRequest(ctx, t.Provider, t.Model, classifyProviderError(err, true, clientGone))
			if tracker != nil && cb.Strategy != "priority" && !clientGone {
				tracker.MarkDrained(t, cb.ParsedDrainTTL(), sanitizeFailure(err))
			}
			return nil
		}
		if err != nil {
			metrics.RecordProviderRequest(ctx, t.Provider, t.Model, classifyProviderError(err, false, false))
			return sanitizedError{err: err, message: sanitizeFailure(err)}
		}
		metrics.RecordProviderRequest(ctx, t.Provider, t.Model, resultSuccess)
		return nil
	})
	attemptResult := resultSuccess
	if err != nil {
		attemptResult = resultFailure
	}
	metrics.RecordCombinationAttempt(r.Context(), cb.Name, attemptResult)
	if err != nil && !responseCommitted {
		writeProviderError(w, err)
	}
}

type sanitizedError struct {
	err     error
	message string
}

func (e sanitizedError) Error() string {
	return e.message
}

func (e sanitizedError) Unwrap() error {
	return e.err
}

func (e sanitizedError) DrainReason() string {
	return e.message
}

// credentialPattern matches secrets that appear as key=value or key: "value"
// pairs. Upstream errors embed credentials inside URL query strings and JSON
var credentialPattern = regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|key|token|secret|password|authorization|bearer)["']?\s*[:=]\s*["']?)([^\s"'&,;)}\]]+)`)

// userInfoPattern matches the password half of a URL's userinfo component,
// e.g. https://user:secret@host/.
var userInfoPattern = regexp.MustCompile(`(://[^/\s:@]+:)([^@\s/]+)(@)`)

func sanitizeFailure(err error) string {
	if err == nil {
		return "upstream failure"
	}
	msg := err.Error()
	// Redact key=value / key: "value" pairs wherever they appear, so a secret
	// buried in a URL query or a JSON blob is caught along with the plain
	// "Bearer <token>" shape.
	msg = credentialPattern.ReplaceAllString(msg, "${1}[redacted]")
	msg = userInfoPattern.ReplaceAllString(msg, "${1}[redacted]${3}")

	fields := strings.Fields(msg)
	redactNext := false
	for i, field := range fields {
		trimmed := strings.Trim(field, `"'(),;`)
		lower := strings.ToLower(trimmed)
		if redactNext || strings.HasPrefix(lower, "sk-") {
			fields[i] = "[redacted]"
			redactNext = false
			continue
		}
		redactNext = lower == "bearer"
	}
	return strings.Join(fields, " ")
}

func providerForName(cfg *config.Config, registry *providerRegistry, name string) (provider.Provider, bool) {
	for _, pc := range cfg.Providers {
		if pc.Name == name {
			if pc.Disabled {
				return nil, false
			}
			return registry.Get(cfg, name)
		}
	}
	return nil, false
}

// writeProviderError maps a provider failure to a client response. Gateway-side
// saturation is a 429 with Retry-After; everything else is a 502. Messages are
// sanitized because upstream errors can embed credentials.
func writeProviderError(w http.ResponseWriter, err error) {
	msg := sanitizeFailure(err)
	var busy interface {
		HTTPStatus() int
		RetryAfter() time.Duration
	}
	if errors.As(err, &busy) && busy.HTTPStatus() == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(int(busy.RetryAfter().Seconds())))
		writeError(w, http.StatusTooManyRequests, msg)
		return
	}
	writeError(w, http.StatusBadGateway, msg)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": statusToCode(status)},
	})
}

func statusToCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid_api_key"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusBadGateway:
		return "upstream_error"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	default:
		return "invalid_request_error"
	}
}
