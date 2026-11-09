package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	// attrObservationType marks a span as a Langfuse generation.
	attrObservationType = "langfuse.observation.type"
	attrSystem          = "gen_ai.system"
	attrRequestModel    = "gen_ai.request.model"
	attrInputTokens     = "gen_ai.usage.input_tokens"
	attrOutputTokens    = "gen_ai.usage.output_tokens"
	attrCacheReadTokens = "gen_ai.usage.cache_read.input_tokens"
	attrCacheWrite      = "gen_ai.usage.cache_creation.input_tokens"
	attrReasoningTokens = "gen_ai.usage.reasoning_tokens"
	attrInput           = "langfuse.observation.input"
	attrOutput          = "langfuse.observation.output"
	attrPrompt          = "gen_ai.prompt"
	attrCompletion      = "gen_ai.completion"
	attrModelParams     = "langfuse.observation.model.parameters"
	attrLevel           = "langfuse.observation.level"
	attrStatusMessage   = "langfuse.observation.status_message"
	attrTraceResult     = "langfuse.trace.metadata.omnigo_result"
	attrTraceCombo      = "langfuse.trace.metadata.omnigo_combo"
	attrTraceAccount    = "langfuse.trace.metadata.omnigo_account"
	attrOpMetadata      = "langfuse.observation.metadata.omnigo_operation"

	// maxCaptureBytes caps each captured prompt or completion. A span
	// attribute is not a transcript store; anything past this is dropped.
	maxCaptureBytes = 64 << 10
	truncatedMarker = "\n…[truncated]"
)

// Generation is one upstream dispatch: the provider call a request actually
// made.
type Generation struct {
	Provider    string
	Model       string
	Combo       string
	Account     string
	Operation   string
	Start       time.Time
	End         time.Time
	Result      string
	Usage       *provider.TokenUsage
	ModelParams map[string]any
	Input       []byte
	Output      []byte
	OutputType  string
}

// RecordGeneration emits one generation span. It parents automatically on the
// span already in ctx, so no trace ids are carried here.
func (t *Tracer) RecordGeneration(ctx context.Context, g Generation) {
	if t == nil || t.tracer == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String(attrObservationType, "generation"),
		attribute.String(attrRequestModel, g.Model),
		attribute.String(attrSystem, g.Provider),
		attribute.String(attrOpMetadata, g.Operation),
		attribute.String(attrStatusMessage, g.Result),
		attribute.String(attrTraceResult, g.Result),
	}
	if g.Combo != "" {
		attrs = append(attrs, attribute.String(attrTraceCombo, g.Combo))
	}
	if g.Account != "" {
		attrs = append(attrs, attribute.String(attrTraceAccount, g.Account))
	}
	if g.Usage != nil {
		if g.Usage.InputTokens > 0 {
			attrs = append(attrs, attribute.Int(attrInputTokens, g.Usage.InputTokens))
		}
		if g.Usage.OutputTokens > 0 {
			attrs = append(attrs, attribute.Int(attrOutputTokens, g.Usage.OutputTokens))
		}
		if g.Usage.CachedTokens > 0 {
			attrs = append(attrs, attribute.Int(attrCacheReadTokens, g.Usage.CachedTokens))
		}
		if g.Usage.CacheCreationInputTokens > 0 {
			attrs = append(attrs, attribute.Int(attrCacheWrite, g.Usage.CacheCreationInputTokens))
		}
		if g.Usage.ReasoningTokens > 0 {
			attrs = append(attrs, attribute.Int(attrReasoningTokens, g.Usage.ReasoningTokens))
		}
	}
	if len(g.ModelParams) > 0 {
		if b, err := json.Marshal(g.ModelParams); err == nil {
			attrs = append(attrs, attribute.String(attrModelParams, string(b)))
		}
	}

	level := "DEFAULT"
	failed := g.Result != "success"
	if failed {
		level = "ERROR"
	}
	attrs = append(attrs, attribute.String(attrLevel, level))

	if t.captures() {
		if in := captureText(g.Input); in != "" {
			attrs = append(attrs, attribute.String(attrInput, in), attribute.String(attrPrompt, in))
		}
		if out := captureText(extractAssistantOutput(g.Output, g.OutputType)); out != "" {
			attrs = append(attrs, attribute.String(attrOutput, out), attribute.String(attrCompletion, out))
		}
	}

	opts := []trace.SpanStartOption{
		trace.WithAttributes(attrs...),
		trace.WithTimestamp(g.Start),
		trace.WithSpanKind(trace.SpanKindClient),
	}
	_, span := t.tracer.Start(ctx, spanName(g.Operation, g.Model), opts...)
	if failed {
		span.SetStatus(codes.Error, g.Result)
	}
	span.End(trace.WithTimestamp(g.End))
}

func (t *Tracer) captures() bool { return t.opts.Capture != nil && t.opts.Capture() }

func spanName(operation, model string) string {
	if operation == "" {
		return "chat." + model
	}
	return operation + "." + model
}

// captureText redacts credentials and caps the size of one captured value.
func captureText(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return ""
	}
	s = provider.RedactCredentials(s)
	if len(s) > maxCaptureBytes {
		s = s[:maxCaptureBytes] + truncatedMarker
	}
	return s
}

// extractAssistantOutput turns a provider response body into the assistant
// message Langfuse renders: JSON bodies decode choices[].message, SSE bodies
// concatenate choices[].delta.content.
func extractAssistantOutput(body []byte, contentType string) []byte {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil
	}
	if strings.Contains(strings.ToLower(contentType), "event-stream") || bytes.HasPrefix(body, []byte("data:")) {
		return extractSSE(body)
	}
	return extractJSON(body)
}

type chatChoice struct {
	Message struct {
		Content   any `json:"content"`
		ToolCalls any `json:"tool_calls"`
	} `json:"message"`
}

type assistantMessage struct {
	Role      string `json:"role"`
	Content   any    `json:"content,omitempty"`
	ToolCalls any    `json:"tool_calls,omitempty"`
}

func extractJSON(body []byte) []byte {
	var resp struct {
		Choices []chatChoice `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Choices) == 0 {
		return nil
	}
	msg := assistantMessage{Role: "assistant", Content: resp.Choices[0].Message.Content, ToolCalls: resp.Choices[0].Message.ToolCalls}
	out, err := json.Marshal(msg)
	if err != nil {
		return nil
	}
	return out
}

func extractSSE(body []byte) []byte {
	type delta struct {
		Content string `json:"content"`
	}
	var sb strings.Builder
	found := false
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta delta `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				sb.WriteString(c.Delta.Content)
				found = true
			}
		}
	}
	if !found {
		return nil
	}
	out, err := json.Marshal(assistantMessage{Role: "assistant", Content: sb.String()})
	if err != nil {
		return nil
	}
	return out
}
