package antigravity

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestToEnvelopeMapsRolesAndParts(t *testing.T) {
	req := provider.ChatRequest{
		Model: "gemini-3.7-flash-medium",
		Messages: []provider.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		},
	}
	env, err := ToEnvelope("proj-1", req.Model, req)
	if err != nil {
		t.Fatalf("ToEnvelope: %v", err)
	}
	if env["project"] != "proj-1" {
		t.Fatalf("project = %v", env["project"])
	}
	if env["model"] != "gemini-3.7-flash-medium" {
		t.Fatalf("model = %v", env["model"])
	}
	reqInner, ok := env["request"].(map[string]any)
	if !ok {
		t.Fatalf("no request envelope: %v", env)
	}
	contents, ok := reqInner["contents"].([]any)
	if !ok || len(contents) != 2 {
		t.Fatalf("contents = %v", reqInner["contents"])
	}
	first := contents[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("role = %v", first["role"])
	}
	parts := first["parts"].([]any)
	if parts[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("parts = %v", parts)
	}
}

func TestToEnvelopeOmitsRequestTypeAgent(t *testing.T) {
	// PR #4229 / oh-my-pi #11742: official Antigravity omits requestType on
	// consumer Cloud Code; sending "agent" buckets the request into false 429s.
	req := provider.ChatRequest{Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	env, err := ToEnvelope("p", req.Model, req)
	if err != nil {
		t.Fatal(err)
	}
	if v, exists := env["requestType"]; exists {
		t.Fatalf("requestType = %v, want omitted", v)
	}
}

func TestToEnvelopeExtractsMultimodalTextParts(t *testing.T) {
	req := provider.ChatRequest{
		Model: "gemini-3.7-flash-medium",
		Messages: []provider.Message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "describe this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/image.png"}},
				map[string]any{"type": "text", "text": "please"},
			},
		}},
	}
	env, err := ToEnvelope("proj-1", req.Model, req)
	if err != nil {
		t.Fatalf("ToEnvelope: %v", err)
	}
	contents := env["request"].(map[string]any)["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["text"] != "describe this" || parts[1].(map[string]any)["text"] != "please" {
		t.Fatalf("parts = %#v", parts)
	}
}

func TestToEnvelopeIgnoresImageOnlyMessage(t *testing.T) {
	req := provider.ChatRequest{
		Model: "gemini-3.7-flash-medium",
		Messages: []provider.Message{
			{Role: "user", Content: []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
			}},
			{Role: "user", Content: "follow-up text"},
		},
	}
	env, err := ToEnvelope("proj-1", req.Model, req)
	if err != nil {
		t.Fatalf("ToEnvelope rejected image-only message: %v", err)
	}
	contents := env["request"].(map[string]any)["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents = %v, want image-only message skipped", contents)
	}
	parts := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "follow-up text" {
		t.Fatalf("parts = %v, want follow-up text", parts)
	}
}

func TestTranslateSSEDelta(t *testing.T) {
	gemini := []byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hel"}]}}]}`)
	out, err := TranslateSSE(gemini)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"delta":{"content":"hel"}`) {
		t.Fatalf("out = %s", out)
	}
	if !strings.HasPrefix(string(out), "data: ") {
		t.Fatalf("out = %s", out)
	}
}

// The daily-cloudcode-pa host wraps every SSE frame in {"response":{...}};
// the legacy host sends it bare. Both must parse.
func TestParseGeminiFrameAcceptsResponseWrapper(t *testing.T) {
	wrapped := []byte(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]}}]}}`)
	body, err := parseGeminiFrame(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	text, _, _ := body.content(0)
	if text != "hello" {
		t.Fatalf("text = %q, want hello", text)
	}
	bare := []byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}]}`)
	body, err = parseGeminiFrame(bare)
	if err != nil {
		t.Fatal(err)
	}
	text, _, _ = body.content(0)
	if text != "hi" {
		t.Fatalf("text = %q, want hi", text)
	}
}

func TestTranslateSSESkipsEmptyCandidate(t *testing.T) {
	gemini := []byte(`data: {"candidates":[]}`)
	out, err := TranslateSSE(gemini)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("expected nil for metadata chunk, got %s", out)
	}
}

func TestTranslateSSEEmitsFinishReasonWithoutText(t *testing.T) {
	gemini := []byte(`data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}]}`)
	out, err := TranslateSSE(gemini)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !strings.Contains(string(out), `"finish_reason":"stop"`) {
		t.Fatalf("finish-only frame was dropped: %s", out)
	}
	if strings.Contains(string(out), `"content"`) {
		t.Fatalf("finish chunk invented content: %s", out)
	}
}

func TestTranslateSSEMapsMaxTokensToLength(t *testing.T) {
	gemini := []byte(`data: {"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":"cut"}]}}]}`)
	out, err := TranslateSSE(gemini)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if !strings.Contains(body, `"content":"cut"`) || !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("out = %s", body)
	}
}

func TestTranslateSSEMapsSafetyToContentFilter(t *testing.T) {
	gemini := []byte(`data: {"candidates":[{"finishReason":"SAFETY","content":{"parts":[]}}]}`)
	out, err := TranslateSSE(gemini)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !strings.Contains(string(out), `"finish_reason":"content_filter"`) {
		t.Fatalf("safety finish was not content_filter: %s", out)
	}
}

func TestParseGeminiFrameAcceptsDataWithoutSpace(t *testing.T) {
	frame := []byte(`data:{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`)
	body, err := parseGeminiFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	text, _, _ := body.content(0)
	if text != "hi" {
		t.Fatalf("text = %q, want hi", text)
	}
}

func TestTranslateSSEForwardsThoughtAsReasoning(t *testing.T) {
	gemini := []byte(`data: {"candidates":[{"content":{"parts":[{"thought":true,"text":"because"}]}}]}`)
	out, err := TranslateSSE(gemini)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if !strings.Contains(body, `"reasoning_content":"because"`) {
		t.Fatalf("thought was dropped: %s", body)
	}
	if strings.Contains(body, `"content":"because"`) {
		t.Fatalf("thought was forwarded as visible content: %s", body)
	}
}

func TestTranslateSSEForwardsFunctionCall(t *testing.T) {
	gemini := []byte(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"weather","args":{"city":"Rome"}}}]},"finishReason":"STOP"}]}`)
	out, err := TranslateSSE(gemini)
	if err != nil {
		t.Fatal(err)
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(out), []byte("data: ")), &chunk); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("chunk = %+v", chunk)
	}
	calls := chunk.Choices[0].Delta.ToolCalls
	if len(calls) != 1 || calls[0].ID != "call_0" || calls[0].Function.Name != "weather" || calls[0].Function.Arguments != `{"city":"Rome"}` {
		t.Fatalf("tool calls = %+v", calls)
	}
}

func TestTranslateSSEIncludesUsageOnTerminalChunk(t *testing.T) {
	gemini := []byte(`data: {"candidates":[{"finishReason":"STOP","content":{"parts":[]}}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":4,"totalTokenCount":15}}`)
	out, err := TranslateSSE(gemini)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if !strings.Contains(body, `"prompt_tokens":11`) || !strings.Contains(body, `"completion_tokens":4`) || !strings.Contains(body, `"total_tokens":15`) {
		t.Fatalf("usage was dropped: %s", body)
	}
}
