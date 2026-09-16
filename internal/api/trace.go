package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// TraceContext represents a W3C Trace Context.
type TraceContext struct {
	TraceID string
	SpanID  string
	Sampled bool
}

type ctxKeyTraceContext struct{}

// WithTraceContext binds a TraceContext to the request context.
func WithTraceContext(ctx context.Context, tc *TraceContext) context.Context {
	return context.WithValue(ctx, ctxKeyTraceContext{}, tc)
}

// TraceContextFromContext retrieves the TraceContext from context.
func TraceContextFromContext(ctx context.Context) *TraceContext {
	if tc, ok := ctx.Value(ctxKeyTraceContext{}).(*TraceContext); ok {
		return tc
	}
	return nil
}

// ParseTraceparent validates and parses a W3C traceparent header.
// Format: version-trace_id-parent_id-trace_flags (e.g. 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01)
func ParseTraceparent(header string) (traceID string, parentID string, sampled bool, ok bool) {
	header = strings.TrimSpace(header)
	if len(header) != 55 {
		return "", "", false, false
	}
	parts := strings.Split(header, "-")
	if len(parts) != 4 {
		return "", "", false, false
	}
	version, trace, parent, flags := parts[0], parts[1], parts[2], parts[3]
	if len(version) != 2 || len(trace) != 32 || len(parent) != 16 || len(flags) != 2 {
		return "", "", false, false
	}
	if !isHex(version) || !isHex(trace) || !isHex(parent) || !isHex(flags) {
		return "", "", false, false
	}
	if trace == "00000000000000000000000000000000" || parent == "0000000000000000" {
		return "", "", false, false
	}
	return strings.ToLower(trace), strings.ToLower(parent), flags == "01", true
}

func isHex(s string) bool {
	for i := range s {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}

func randomTraceHex(bytesLen int) string {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", bytesLen*2, time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// NewTraceContext extracts inbound W3C traceparent or generates a fresh trace context.
func NewTraceContext(inbound string) *TraceContext {
	traceID, _, _, ok := ParseTraceparent(inbound)
	if !ok {
		traceID = randomTraceHex(16)
	}
	spanID := randomTraceHex(8)
	return &TraceContext{
		TraceID: strings.ToLower(traceID),
		SpanID:  strings.ToLower(spanID),
		Sampled: true,
	}
}

// Traceparent formats the W3C traceparent header.
func (tc *TraceContext) Traceparent() string {
	flags := "00"
	if tc.Sampled {
		flags = "01"
	}
	return fmt.Sprintf("00-%s-%s-%s", tc.TraceID, tc.SpanID, flags)
}

// FormatServerTiming formats OpenTelemetry GenAI semantic convention attributes for W3C Server-Timing.
func FormatServerTiming(provider, model string, dur time.Duration) string {
	durMs := float64(dur.Microseconds()) / 1000.0
	return fmt.Sprintf("gen_ai.system;desc=%q, gen_ai.response.model;desc=%q, dur=%.2f", provider, model, durMs)
}

// SetTelemetryHeaders writes W3C traceparent, Server-Timing, and X-OmniGo metadata headers.
func SetTelemetryHeaders(w http.ResponseWriter, tc *TraceContext, provider, model string, startTime time.Time) {
	if tc != nil {
		w.Header().Set("traceparent", tc.Traceparent())
	}
	if provider != "" {
		w.Header().Set("X-OmniGo-Provider", provider)
	}
	if model != "" {
		w.Header().Set("X-OmniGo-Model", model)
	}
	if provider != "" || model != "" {
		w.Header().Set("Server-Timing", FormatServerTiming(provider, model, time.Since(startTime)))
	}
}
