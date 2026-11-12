package commandcode

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func parsedBody(t *testing.T, raw map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	parsed := provider.ParseBody(b)
	if parsed == nil {
		t.Fatal("ParseBody returned nil")
	}
	return parsed
}

func TestTranslateExtractsSystemAndDefaults(t *testing.T) {
	parsed := parsedBody(t, map[string]any{
		"model": "go-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	now := func() time.Time { return time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC) }
	body, err := openAIToCommandCode(provider.ChatRequest{Parsed: parsed}, "go-model", "thread-1", now)
	if err != nil {
		t.Fatalf("openAIToCommandCode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["threadId"] != "thread-1" {
		t.Fatalf("threadId = %v", got["threadId"])
	}
	params := got["params"].(map[string]any)
	if params["system"] != "be brief" {
		t.Fatalf("system = %v", params["system"])
	}
	if params["stream"] != true {
		t.Fatalf("stream = %v", params["stream"])
	}
	if params["max_tokens"] != float64(64000) && params["max_tokens"] != 64000 {
		t.Fatalf("max_tokens default = %v", params["max_tokens"])
	}
	if params["temperature"] != 0.3 {
		t.Fatalf("temperature default = %v", params["temperature"])
	}
	msgs := params["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v (system excluded)", params["messages"])
	}
}

func TestTranslateUserAssistantToolRoundTrip(t *testing.T) {
	imageData := "aGVsbG8="
	parsed := parsedBody(t, map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "thinking", "reasoning_content": "rc", "tool_calls": []any{
				map[string]any{"id": "call-1", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"x"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "sunny"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "look"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + imageData}},
			}},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "get_weather", "description": "d", "parameters": map[string]any{"type": "object"},
		}}},
		"reasoning_effort": "high",
	})
	body, err := openAIToCommandCode(provider.ChatRequest{Parsed: parsed}, "m", "t", nil)
	if err != nil {
		t.Fatalf("openAIToCommandCode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	params := got["params"].(map[string]any)
	if params["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", params["reasoning_effort"])
	}
	tools := params["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tools = %v", tools)
	}
	if _, ok := tools[0].(map[string]any)["input_schema"]; !ok {
		t.Fatal("tool missing input_schema")
	}
	msgs := params["messages"].([]any)
	asst := msgs[0].(map[string]any)["content"].([]any)
	if len(asst) != 3 {
		t.Fatalf("assistant blocks = %v", asst)
	}
	toolResult := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolResult["toolName"] != "get_weather" || toolResult["toolCallId"] != "call-1" {
		t.Fatalf("tool result = %v", toolResult)
	}
	userBlocks := msgs[2].(map[string]any)["content"].([]any)
	if len(userBlocks) != 2 || userBlocks[1].(map[string]any)["mimeType"] != "image/png" {
		t.Fatalf("user blocks = %v", userBlocks)
	}
}

func TestTranslateToolFallbackNameAndParams(t *testing.T) {
	parsed := parsedBody(t, map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "tool", "tool_call_id": "call-9", "content": "x"},
		},
		"max_tokens":  100,
		"temperature": 0.9,
		"top_p":       0.5,
	})
	body, err := openAIToCommandCode(provider.ChatRequest{Parsed: parsed}, "m", "t", nil)
	if err != nil {
		t.Fatalf("openAIToCommandCode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	params := got["params"].(map[string]any)
	if v := params["max_tokens"]; v != float64(100) && v != 100 {
		t.Fatalf("max_tokens = %v", v)
	}
	if params["temperature"] != 0.9 || params["top_p"] != 0.5 {
		t.Fatalf("params = %v", params)
	}
	msgs := params["messages"].([]any)
	toolResult := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolResult["toolName"] != "unknown" {
		t.Fatalf("toolName fallback = %v", toolResult)
	}
}
