package tracing

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newTestTracer(t *testing.T, capture bool) (*Tracer, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	enabled := true
	tr := newWithProcessor(Options{
		Enabled: func() bool { return enabled },
		Capture: func() bool { return capture },
	}, rec)
	t.Cleanup(func() { _ = tr.Shutdown(context.Background()) })
	return tr, rec
}

func attrValue(t *testing.T, span sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	t.Helper()
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func sampleGeneration() Generation {
	start := time.Now().Add(-2 * time.Second)
	return Generation{
		Provider:  "openai-main",
		Model:     "gpt-4o",
		Combo:     "auto",
		Account:   "acct-1",
		Operation: "chat",
		Start:     start,
		End:       start.Add(time.Second),
		Result:    "success",
		Usage:     &provider.TokenUsage{InputTokens: 11, OutputTokens: 4, CachedTokens: 3, ReasoningTokens: 2, Present: true},
		ModelParams: map[string]any{
			"temperature": 0.2,
			"stream":      true,
		},
		Input: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
}

func TestRecordGenerationMapsAttributes(t *testing.T) {
	tr, rec := newTestTracer(t, false)
	tr.RecordGeneration(context.Background(), sampleGeneration())

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "chat.gpt-4o" {
		t.Errorf("name = %q", span.Name())
	}
	want := map[string]string{
		attrObservationType: "generation",
		attrRequestModel:    "gpt-4o",
		attrSystem:          "openai-main",
		attrTraceCombo:      "auto",
		attrTraceResult:     "success",
	}
	for k, v := range want {
		got, ok := attrValue(t, span, k)
		if !ok || got.AsString() != v {
			t.Errorf("%s = %q (present=%v), want %q", k, got.AsString(), ok, v)
		}
	}
	if v, _ := attrValue(t, span, attrInputTokens); v.AsInt64() != 11 {
		t.Errorf("input tokens = %d", v.AsInt64())
	}
	if v, _ := attrValue(t, span, attrOutputTokens); v.AsInt64() != 4 {
		t.Errorf("output tokens = %d", v.AsInt64())
	}
	if v, _ := attrValue(t, span, attrCacheReadTokens); v.AsInt64() != 3 {
		t.Errorf("cached tokens = %d", v.AsInt64())
	}
	if _, ok := attrValue(t, span, attrInput); ok {
		t.Error("input captured with capture_content off")
	}
	if _, ok := attrValue(t, span, attrOutput); ok {
		t.Error("output captured with capture_content off")
	}
}

func TestRecordGenerationErrorLevel(t *testing.T) {
	tr, rec := newTestTracer(t, false)
	g := sampleGeneration()
	g.Result = "provider_error"
	g.Usage = nil
	tr.RecordGeneration(context.Background(), g)

	span := rec.Ended()[0]
	if v, _ := attrValue(t, span, attrLevel); v.AsString() != "ERROR" {
		t.Errorf("level = %q, want ERROR", v.AsString())
	}
	if _, ok := attrValue(t, span, attrInputTokens); ok {
		t.Error("zero usage set token attributes")
	}
}

func TestRecordGenerationCapturesAndRedacts(t *testing.T) {
	tr, rec := newTestTracer(t, true)
	g := sampleGeneration()
	g.Input = []byte(`{"api_key":"sk-live-abcdef","messages":[]}`)
	g.Output = []byte(`{"choices":[{"message":{"content":"hello there"}}]}`)
	g.OutputType = "application/json"
	tr.RecordGeneration(context.Background(), g)

	span := rec.Ended()[0]
	in, ok := attrValue(t, span, attrInput)
	if !ok {
		t.Fatal("input not captured")
	}
	if strings.Contains(in.AsString(), "sk-live-abcdef") {
		t.Errorf("credential leaked: %s", in.AsString())
	}
	out, ok := attrValue(t, span, attrOutput)
	if !ok {
		t.Fatal("output not captured")
	}
	var msg assistantMessage
	if err := json.Unmarshal([]byte(out.AsString()), &msg); err != nil {
		t.Fatalf("output is not JSON: %v (%s)", err, out.AsString())
	}
	if msg.Content != "hello there" || msg.Role != "assistant" {
		t.Errorf("message = %+v", msg)
	}
}

func TestRecordGenerationTruncates(t *testing.T) {
	tr, rec := newTestTracer(t, true)
	g := sampleGeneration()
	g.Input = []byte(strings.Repeat("a", 70<<10))
	tr.RecordGeneration(context.Background(), g)

	in, _ := attrValue(t, rec.Ended()[0], attrInput)
	if len(in.AsString()) > maxCaptureBytes+len(truncatedMarker) {
		t.Errorf("captured %d bytes, want <= %d", len(in.AsString()), maxCaptureBytes+len(truncatedMarker))
	}
	if !strings.HasSuffix(in.AsString(), truncatedMarker) {
		t.Error("missing truncation marker")
	}
}

func TestExtractSSEOutput(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
		": keepalive\n\ndata: [DONE]\n\n")
	out := extractAssistantOutput(body, "text/event-stream")
	var msg assistantMessage
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	if msg.Content != "Hello" {
		t.Errorf("content = %v", msg.Content)
	}
}

func TestExtractOutputRejectsGarbage(t *testing.T) {
	if got := extractAssistantOutput([]byte("not json"), "application/json"); got != nil {
		t.Errorf("got %s, want nil", got)
	}
	if got := extractAssistantOutput(nil, ""); got != nil {
		t.Errorf("got %s, want nil", got)
	}
}
