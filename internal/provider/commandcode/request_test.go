package commandcode

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func decodeEnvelope(t *testing.T, req provider.ChatRequest) map[string]any {
	t.Helper()
	raw, err := buildEnvelope(req, "thread-1")
	if err != nil {
		t.Fatalf("buildEnvelope failed: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	return envelope
}

func TestEnvelopeHoistsSystemAndBlocks(t *testing.T) {
	req := provider.ChatRequest{
		Model: "deepseek/deepseek-v4-pro",
		Parsed: map[string]any{
			"messages": []any{
				map[string]any{"role": "system", "content": "be brief"},
				map[string]any{"role": "developer", "content": "and kind"},
				map[string]any{"role": "user", "content": "hi"},
			},
			"temperature": 0.9,
		},
	}
	envelope := decodeEnvelope(t, req)
	params := envelope["params"].(map[string]any)

	if got := params["system"]; got != "be brief\n\nand kind" {
		t.Errorf("system = %v", got)
	}
	if got := params["temperature"]; got != 0.9 {
		t.Errorf("temperature = %v, want 0.9", got)
	}
	if got := params["stream"]; got != true {
		t.Errorf("stream = %v, want true", got)
	}

	messages := params["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("system messages must be hoisted out, got %d messages", len(messages))
	}
	user := messages[0].(map[string]any)
	blocks, ok := user["content"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("content must be typed blocks, got %v", user["content"])
	}
	if block := blocks[0].(map[string]any); block["type"] != "text" || block["text"] != "hi" {
		t.Errorf("unexpected block %v", block)
	}
}

func TestEnvelopeToolCallsAndResults(t *testing.T) {
	req := provider.ChatRequest{
		Model: "x/y",
		Parsed: map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "read it"},
				map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
					map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
						"name": "read_file", "arguments": `{"path":"a.txt"}`,
					}},
				}},
				map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "contents"},
			},
			"tools": []any{
				map[string]any{"type": "function", "function": map[string]any{
					"name": "read_file", "description": "read", "parameters": map[string]any{"type": "object"},
				}},
			},
		},
	}
	params := decodeEnvelope(t, req)["params"].(map[string]any)

	tools := params["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "read_file" {
		t.Errorf("tool name = %v", tool["name"])
	}
	if _, ok := tool["input_schema"]; !ok {
		t.Error("tool must carry input_schema")
	}

	messages := params["messages"].([]any)
	assistant := messages[1].(map[string]any)
	blocks := assistant["content"].([]any)
	foundCall := false
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block["type"] == "tool-call" {
			foundCall = true
			if block["toolName"] != "read_file" || block["toolCallId"] != "call_1" {
				t.Errorf("unexpected tool-call %v", block)
			}
		}
	}
	if !foundCall {
		t.Error("assistant tool_calls must become tool-call blocks")
	}

	toolMessage := messages[2].(map[string]any)
	result := toolMessage["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool-result" || result["toolName"] != "read_file" {
		t.Errorf("unexpected tool-result %v", result)
	}
}

func TestStateTranslatesEvents(t *testing.T) {
	state := newState("x/y")
	events := []map[string]any{
		{"type": "reasoning-delta", "text": "thinking"},
		{"type": "text-delta", "text": "hello "},
		{"type": "text-delta", "text": "world"},
		{"type": "tool-input-start", "id": "t1", "toolName": "read_file"},
		{"type": "tool-input-delta", "id": "t1", "delta": `{"path":`},
		{"type": "tool-input-delta", "id": "t1", "delta": `"a"}`},
		{"type": "finish-step", "finishReason": "tool-calls", "usage": map[string]any{
			"inputTokens": float64(10), "outputTokens": float64(5),
		}},
		{"type": "finish", "finishReason": "tool-calls"},
	}

	var contents, reasonings []string
	for _, event := range events {
		if _, err := state.consume(event); err != nil {
			t.Fatalf("consume(%v) failed: %v", event["type"], err)
		}
	}
	if state.text.String() != "hello world" {
		t.Errorf("text = %q", state.text.String())
	}
	if state.reasoning.String() != "thinking" {
		t.Errorf("reasoning = %q", state.reasoning.String())
	}
	if !state.finished {
		t.Error("expected finished state")
	}
	if state.finish != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", state.finish)
	}
	if state.usage.input != 10 || state.usage.output != 5 {
		t.Errorf("usage = %+v", state.usage)
	}
	if len(state.tools) != 1 || state.tools[0].arguments.String() != `{"path":"a"}` {
		t.Errorf("tools = %+v", state.tools)
	}
	_ = contents
	_ = reasonings
}

func TestStateErrorEvent(t *testing.T) {
	state := newState("x/y")
	_, err := state.consume(map[string]any{
		"type":  "error",
		"error": map[string]any{"message": "rate limit exceeded"},
	})
	if err == nil {
		t.Fatal("expected error from error event")
	}
	var statusErr *provider.HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != 429 {
		t.Fatalf("expected 429, got %v", err)
	}
}
