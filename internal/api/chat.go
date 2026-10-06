package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/cache"
	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func handleChat(getCfg func() *config.Config, registry *providerRegistry, tracker *combo.Tracker, m Metrics, responseCache cache.CacheBackend) http.HandlerFunc {
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
		parsed := provider.ParseBody(raw)
		if parsed == nil {
			writeError(w, http.StatusBadRequest, "model is required")
			return
		}
		model, _ := parsed["model"].(string)
		if model == "" {
			writeError(w, http.StatusBadRequest, "model is required")
			return
		}
		stream, _ := parsed["stream"].(bool)
		msgs := messagesFromParsed(parsed)
		cfg := getCfg()
		req := provider.ChatRequest{Model: model, Stream: stream, Messages: msgs, Raw: raw, Parsed: parsed}

		cb, comboOK := findCombo(cfg, model)

		cacheable := responseCache != nil && cfg != nil && cfg.Cache.EnabledOrDefault() && !cacheBypass(r, parsed, stream, comboOK)
		if comboOK {
			runCombo(w, r, cfg, registry, cb, req, tracker, tc, startTime, metrics)
			return
		}
		if !cacheable {
			serveDirect(w, r, cfg, registry, req, model, metrics, startTime, tc)
			return
		}

		p, provName, resolvedModel, ok := resolveDirect(cfg, registry, model)
		if !ok {
			writeError(w, http.StatusNotFound, "model not found: "+model)
			return
		}
		req.Model = resolvedModel
		callerID := auth.CallerFrom(r.Context()).ID()
		normal, hashable := cacheMessages(msgs)
		temp, maxTokens, deterministic := cacheParams(parsed)
		if callerID == "" || !hashable || !deterministic {
			serveResolved(w, r, p, provName, resolvedModel, req, metrics, startTime, tc)
			return
		}
		key, keyErr := cache.ComputeCacheKey(callerID, resolvedModel, normal, temp, maxTokens)
		if keyErr != nil {
			serveResolved(w, r, p, provName, resolvedModel, req, metrics, startTime, tc)
			return
		}
		cached, getErr := responseCache.Get(r.Context(), key)
		if getErr == nil {
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(cached)
			metrics.RecordCacheHit(r.Context(), resolvedModel)
			return
		}
		if !errors.Is(getErr, cache.ErrNotFound) && !errors.Is(getErr, cache.ErrExpired) {
			serveResolved(w, r, p, provName, resolvedModel, req, metrics, startTime, tc)
			return
		}

		w.Header().Set("X-Cache", "MISS")
		metrics.RecordCacheMiss(r.Context(), resolvedModel)
		SetTelemetryHeaders(w, tc, provName, resolvedModel, startTime)
		recorded := &bodyRecorder{ResponseWriter: w}
		providerCtx := withTokenUsage(r.Context(), metrics, provName, resolvedModel, "")
		providerErr := p.ChatCompletion(providerCtx, req, recorded)
		metrics.RecordProviderRequest(r.Context(), provName, resolvedModel, classifyProviderError(providerErr, false, r.Context().Err() != nil))
		if providerErr == nil && recorded.statusCode() == http.StatusOK && recorded.body.Len() > 0 && r.Context().Err() == nil {
			_ = responseCache.Set(context.WithoutCancel(r.Context()), key, recorded.body.Bytes(), cfg.Cache.ParsedTTL())
		}
		copyRecorded(w, recorded)
		if providerErr != nil && recorded.body.Len() == 0 && !recorded.wrote {
			writeProviderError(w, providerErr)
		}
	}
}

func serveDirect(w http.ResponseWriter, r *http.Request, cfg *config.Config, registry *providerRegistry, req provider.ChatRequest, model string, metrics Metrics, startTime time.Time, tc *TraceContext) {
	p, provName, resolvedModel, ok := resolveDirect(cfg, registry, model)
	if !ok {
		writeError(w, http.StatusNotFound, "model not found: "+model)
		return
	}
	req.Model = resolvedModel
	serveResolved(w, r, p, provName, resolvedModel, req, metrics, startTime, tc)
}

func serveResolved(w http.ResponseWriter, r *http.Request, p provider.Provider, provName, resolvedModel string, req provider.ChatRequest, metrics Metrics, startTime time.Time, tc *TraceContext) {
	SetTelemetryHeaders(w, tc, provName, resolvedModel, startTime)
	// Providers stream straight to the client on this path, so a late failure
	// must not be answered with a fresh JSON error envelope: the SSE body is
	// already on the wire and the object would be parsed as a bad frame.
	tracked := &commitTracker{ResponseWriter: w}
	providerCtx := withTokenUsage(r.Context(), metrics, provName, resolvedModel, "")
	providerErr := p.ChatCompletion(providerCtx, req, tracked)
	metrics.RecordProviderRequest(r.Context(), provName, resolvedModel, classifyProviderError(providerErr, tracked.committed, r.Context().Err() != nil))
	if providerErr != nil && !tracked.committed {
		writeProviderError(w, providerErr)
	} else if providerErr != nil && req.Stream && r.Context().Err() == nil {
		writeStreamError(w, sanitizeFailure(providerErr))
	}
}

func copyRecorded(w http.ResponseWriter, recorded *bodyRecorder) {
	if recorded.body.Len() == 0 && !recorded.wrote {
		return
	}
	w.WriteHeader(recorded.statusCode())
	if recorded.body.Len() > 0 {
		_, _ = w.Write(recorded.body.Bytes())
	}
}

func messagesFromParsed(parsed map[string]any) []provider.Message {
	rawMessages, ok := parsed["messages"].([]any)
	if !ok {
		return nil
	}
	messages := make([]provider.Message, 0, len(rawMessages))
	for _, raw := range rawMessages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		messages = append(messages, provider.Message{Role: role, Content: message["content"]})
	}
	return messages
}

func cacheMessages(msgs []provider.Message) ([]cache.NormalMessage, bool) {
	if msgs == nil {
		return nil, true
	}
	out := make([]cache.NormalMessage, 0, len(msgs))
	for _, msg := range msgs {
		content, ok := msg.Content.(string)
		if !ok {
			return nil, false
		}
		out = append(out, cache.NormalMessage{Role: msg.Role, Content: content})
	}
	return out, true
}

func cacheParams(parsed map[string]any) (temp float64, maxTokens int, deterministic bool) {
	temp, tempOK := cacheFloat(parsed["temperature"], true)
	if !tempOK || temp != 0 {
		return 0, 0, false
	}
	maxTokens, maxOK := cacheInt(parsed["max_tokens"])
	if !maxOK {
		return temp, 0, false
	}
	return temp, maxTokens, true
}

func cacheFloat(v any, absentOK bool) (float64, bool) {
	if v == nil {
		return 0, absentOK
	}
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

func cacheInt(v any) (int, bool) {
	if v == nil {
		return 0, true
	}
	f, ok := cacheFloat(v, false)
	if !ok || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

func cacheExactPayload(parsed map[string]any) bool {
	for key := range parsed {
		switch key {
		case "model", "messages", "temperature", "max_tokens", provider.OpenAIBodyCacheKey():
		default:
			return false
		}
	}
	return true
}

func cacheExactMessages(raw any) bool {
	if raw == nil {
		return true
	}
	messages, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			return false
		}
		for key := range message {
			switch key {
			case "role", "content":
			default:
				return false
			}
		}
	}
	return true
}

func cacheBypass(r *http.Request, parsed map[string]any, stream bool, combo bool) bool {
	if !cacheExactPayload(parsed) {
		return true
	}
	if !cacheExactMessages(parsed["messages"]) {
		return true
	}
	if combo || stream {
		return true
	}
	if _, _, deterministic := cacheParams(parsed); !deterministic {
		return true
	}
	if tools, ok := parsed["tools"]; ok && tools != nil {
		list, isList := tools.([]any)
		if !isList || len(list) > 0 {
			return true
		}
	}
	if choice, ok := parsed["tool_choice"]; ok && choice != nil {
		return true
	}
	rawMessages, ok := parsed["messages"].([]any)
	if ok {
		for _, raw := range rawMessages {
			message, isMap := raw.(map[string]any)
			if !isMap {
				return true
			}
			role, roleOK := message["role"].(string)
			if !roleOK || strings.TrimSpace(role) == "" || role == "tool" || role == "function" {
				return true
			}
			if _, isString := message["content"].(string); !isString && message["content"] != nil {
				return true
			}
		}
	}
	if cacheControlNoCache(r.Header.Get("Cache-Control")) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Bypass-Cache")), "true") {
		return true
	}
	if _, hashable := cacheMessages(messagesFromParsed(parsed)); !hashable {
		return true
	}
	return false
}

func cacheControlNoCache(header string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "no-cache") {
			return true
		}
	}
	return false
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
				if pc.Disabled || !pc.HasModel(modelID) || pc.IsModelDisabled(modelID) {
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
		Timeout:  cb.ParsedTimeout(),
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
		providerCtx := withTokenUsage(ctx, metrics, t.Provider, t.Model, cb.Name)
		providerErr := p.ChatCompletion(providerCtx, req, attempt)
		err := attempt.finish(providerErr)
		if errors.Is(err, errResponseCommitted) {
			responseCommitted = true
			// A client abort mid-stream cancels the request context; that is not an
			// upstream fault, so the target stays healthy. A deadline this process
			// imposed on the attempt is not one either. A stalled upstream leaves
			// both contexts alive and is still drained.
			clientGone := r.Context().Err() != nil
			gatewayDeadline := ctx.Err() != nil && !clientGone
			metrics.RecordProviderRequest(ctx, t.Provider, t.Model, classifyProviderError(err, true, clientGone))
			if tracker != nil && cb.Strategy != "priority" && !clientGone && !gatewayDeadline {
				tracker.MarkDrained(t, cb.ParsedDrainTTL(), sanitizeFailure(err))
			}
			// The client already has part of the answer and no failover is
			// possible, so say the generation failed instead of ending the
			// stream as if it had completed.
			if req.Stream && !clientGone {
				writeStreamError(w, sanitizeFailure(err))
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

// withTokenUsage installs the sink that turns one completed upstream response
// into token counters. combo is empty on a direct call. model is the id that
// was dispatched: a combo target, or the bare model on a direct call.
func withTokenUsage(ctx context.Context, metrics Metrics, providerName, model, combo string) context.Context {
	return provider.WithUsageSink(ctx, func(usage provider.TokenUsage) {
		metrics.RecordTokenUsage(ctx, providerName, model, usage.Account, combo, usage)
	})
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

func sanitizeFailure(err error) string {
	if err == nil {
		return "upstream failure"
	}
	// Upstream error text can embed credentials; the shared redactor covers
	// key=value pairs, URL userinfo, and Bearer/sk- shapes.
	return provider.RedactCredentials(err.Error())
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
	if errors.Is(err, combo.ErrComboTimeout) || errors.Is(err, context.DeadlineExceeded) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusGatewayTimeout, msg)
		return
	}
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
	case http.StatusGatewayTimeout:
		return "timeout"
	default:
		return "invalid_request_error"
	}
}
