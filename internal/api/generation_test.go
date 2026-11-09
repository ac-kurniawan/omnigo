package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// usageRecorder is a full Metrics implementation that keeps the token counters
// trackAttempt delivers.
type usageRecorder struct {
	usage []provider.TokenUsage
}

func (m *usageRecorder) RecordProviderRequest(context.Context, string, string, string) {}
func (m *usageRecorder) RecordCombinationAttempt(context.Context, string, string)      {}
func (m *usageRecorder) RecordSmartDecision(context.Context, string, string, string)   {}
func (m *usageRecorder) RecordTokenUsage(_ context.Context, _, _, _, _ string, u provider.TokenUsage) {
	m.usage = append(m.usage, u)
}
func (m *usageRecorder) RecordCacheHit(context.Context, string)       {}
func (m *usageRecorder) RecordCacheMiss(context.Context, string)      {}
func (m *usageRecorder) RecordGeneration(context.Context, Generation) {}

func TestModelParamsKeepsOnlyRequestKnobs(t *testing.T) {
	req := provider.ChatRequest{Parsed: map[string]any{
		"temperature":                 0.2,
		"max_tokens":                  float64(64),
		"stream":                      true,
		"messages":                    []any{},
		provider.OpenAIBodyCacheKey(): "payload",
	}}
	got := modelParams(req)
	if len(got) != 3 {
		t.Fatalf("params = %v, want exactly temperature, max_tokens, stream", got)
	}
	for _, key := range []string{"temperature", "max_tokens", "stream"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
}

func TestTrackAttemptWithoutCaptureReturnsOriginalWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	m := &usageRecorder{}
	_, providerCtx, w := trackAttempt(context.Background(), m, "openai-main", "gpt-4o", "", rec, false)
	if w != http.ResponseWriter(rec) {
		t.Fatal("writer wrapped with capture off")
	}
	provider.ReportUsage(providerCtx, provider.TokenUsage{InputTokens: 7, Present: true})
	if len(m.usage) != 1 || m.usage[0].InputTokens != 7 {
		t.Fatalf("usage = %+v", m.usage)
	}
}

func TestGenerationFromReadsCapture(t *testing.T) {
	rec := httptest.NewRecorder()
	m := &usageRecorder{}
	now := time.Now()
	req := provider.ChatRequest{Raw: []byte(`{"messages":[]}`)}

	cap, providerCtx, w := trackAttempt(context.Background(), m, "openai-main", "gpt-4o", "auto", rec, true)
	provider.ReportUsage(providerCtx, provider.TokenUsage{InputTokens: 5, Account: "acct", Present: true})
	_, _ = w.Write([]byte("body"))

	off := generationFrom(now, now, "chat", "openai-main", "gpt-4o", "auto", cap, resultSuccess, req, false)
	if off.Account != "acct" || off.Usage == nil || off.Usage.InputTokens != 5 {
		t.Fatalf("usage = %+v", off)
	}
	if off.Input != nil || off.Output != nil {
		t.Fatalf("content captured with capture off: %+v", off)
	}

	on := generationFrom(now, now, "chat", "openai-main", "gpt-4o", "auto", cap, resultSuccess, req, true)
	if string(on.Input) != `{"messages":[]}` || string(on.Output) != "body" {
		t.Fatalf("content not captured: input=%q output=%q", on.Input, on.Output)
	}
}

func TestObservingWriterForwardsAndBuffers(t *testing.T) {
	dst := httptest.NewRecorder()
	ow := &observingWriter{ResponseWriter: dst}
	ow.Header().Set("Content-Type", "text/event-stream")
	ow.WriteHeader(http.StatusOK)
	if _, err := ow.Write([]byte("data: hi\n\n")); err != nil {
		t.Fatal(err)
	}
	ow.Flush()

	if dst.Body.String() != "data: hi\n\n" {
		t.Errorf("forwarded body = %q", dst.Body.String())
	}
	if !dst.Flushed {
		t.Error("Flush not forwarded")
	}
	if string(ow.bytes()) != "data: hi\n\n" {
		t.Errorf("buffered = %q", ow.bytes())
	}
	if ow.contentType() != "text/event-stream" {
		t.Errorf("content type = %q", ow.contentType())
	}
	if ow.Unwrap() != http.ResponseWriter(dst) {
		t.Error("Unwrap did not expose the destination")
	}
}

func TestObservingWriterStopsBufferingAtCap(t *testing.T) {
	dst := httptest.NewRecorder()
	ow := &observingWriter{ResponseWriter: dst}
	chunk := make([]byte, 64<<10)
	for range 6 {
		_, _ = ow.Write(chunk)
	}
	if dst.Body.Len() != 6*len(chunk) {
		t.Errorf("forwarded %d bytes, want %d", dst.Body.Len(), 6*len(chunk))
	}
	if len(ow.bytes()) > maxObservedBody {
		t.Errorf("buffered %d bytes, cap %d", len(ow.bytes()), maxObservedBody)
	}
}
