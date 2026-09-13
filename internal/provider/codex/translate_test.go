package codex

import (
	"errors"
	"reflect"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestToResponsesRequestMapsInstructionsAndMultiTurnText(t *testing.T) {
	req := provider.ChatRequest{
		Model: "gpt-5.2-codex",
		Raw: []byte(`{
			"model":"ignored",
			"messages":[
				{"role":"system","content":"first"},
				{"role":"developer","content":[{"type":"text","text":"second"},{"type":"text","text":"third"}]},
				{"role":"user","content":"hello"},
				{"role":"assistant","content":"hi"},
				{"role":"user","content":"again"}
			],
			"temperature":0.2,
			"max_tokens":100
		}`),
	}

	got, err := ToResponsesRequest(req)
	if err != nil {
		t.Fatalf("ToResponsesRequest: %v", err)
	}
	want := map[string]any{
		"model":        "gpt-5.2-codex",
		"instructions": "first\nsecond\nthird",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello"}}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "hi"}}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "again"}}},
		},
		"store":   false,
		"stream":  true,
		"include": []any{"reasoning.encrypted_content"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request = %#v\nwant %#v", got, want)
	}
}

func TestToResponsesRequestUsesParsedMessagesWithoutRawBody(t *testing.T) {
	req := provider.ChatRequest{
		Model:    "codex",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
	got, err := ToResponsesRequest(req)
	if err != nil {
		t.Fatalf("ToResponsesRequest: %v", err)
	}
	input := got["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["role"] != "user" {
		t.Fatalf("input = %#v", input)
	}
}

func TestToResponsesRequestMapsMultipartTextAndImages(t *testing.T) {
	req := provider.ChatRequest{Model: "codex", Raw: []byte(`{
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"look"},
				{"type":"image_url","image_url":{"url":"https://example.invalid/a.png","detail":"high"}},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
			]},
			{"role":"assistant","content":[{"type":"text","text":"seen"}]}
		]
	}`)}

	got, err := ToResponsesRequest(req)
	if err != nil {
		t.Fatalf("ToResponsesRequest: %v", err)
	}
	input := got["input"].([]any)
	user := input[0].(map[string]any)["content"].([]any)
	wantUser := []any{
		map[string]any{"type": "input_text", "text": "look"},
		map[string]any{"type": "input_image", "image_url": "https://example.invalid/a.png", "detail": "high"},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
	}
	if !reflect.DeepEqual(user, wantUser) {
		t.Fatalf("user content = %#v, want %#v", user, wantUser)
	}
	assistant := input[1].(map[string]any)["content"].([]any)
	wantAssistant := []any{map[string]any{"type": "output_text", "text": "seen"}}
	if !reflect.DeepEqual(assistant, wantAssistant) {
		t.Fatalf("assistant content = %#v, want %#v", assistant, wantAssistant)
	}
}

func TestToResponsesRequestMapsToolsCallsAndOutputs(t *testing.T) {
	req := provider.ChatRequest{Model: "codex", Raw: []byte(`{
		"messages":[
			{"role":"user","content":"weather"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"{\"temp\":20}"}
		]
	}`)}

	got, err := ToResponsesRequest(req)
	if err != nil {
		t.Fatalf("ToResponsesRequest: %v", err)
	}
	want := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "weather"}}},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "weather", "arguments": `{"city":"Paris"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": `{"temp":20}`},
	}
	if !reflect.DeepEqual(got["input"], want) {
		t.Fatalf("input = %#v, want %#v", got["input"], want)
	}
}

func TestToResponsesRequestMapsToolsReasoningAndResponseFormat(t *testing.T) {
	req := provider.ChatRequest{Model: "codex", Raw: []byte(`{
		"messages":[],
		"tools":[{"type":"function","function":{
			"name":"lookup","description":"Find a value",
			"parameters":{"type":"object","properties":{"id":{"type":"string"}}},"strict":true
		}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"parallel_tool_calls":false,
		"reasoning_effort":"high",
		"response_format":{"type":"json_schema","json_schema":{
			"name":"answer","description":"An answer","schema":{"type":"object"},"strict":true
		}}
	}`)}

	got, err := ToResponsesRequest(req)
	if err != nil {
		t.Fatalf("ToResponsesRequest: %v", err)
	}
	wantTools := []any{map[string]any{
		"type": "function", "name": "lookup", "description": "Find a value",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}},
		"strict":     true,
	}}
	if !reflect.DeepEqual(got["tools"], wantTools) {
		t.Fatalf("tools = %#v, want %#v", got["tools"], wantTools)
	}
	if !reflect.DeepEqual(got["tool_choice"], map[string]any{"type": "function", "name": "lookup"}) {
		t.Fatalf("tool_choice = %#v", got["tool_choice"])
	}
	if got["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v", got["parallel_tool_calls"])
	}
	if !reflect.DeepEqual(got["reasoning"], map[string]any{"effort": "high", "summary": "auto"}) {
		t.Fatalf("reasoning = %#v", got["reasoning"])
	}
	wantText := map[string]any{"format": map[string]any{
		"type": "json_schema", "name": "answer", "description": "An answer",
		"schema": map[string]any{"type": "object"}, "strict": true,
	}}
	if !reflect.DeepEqual(got["text"], wantText) {
		t.Fatalf("text = %#v, want %#v", got["text"], wantText)
	}
}

func TestToResponsesRequestFiltersReasoningObject(t *testing.T) {
	req := provider.ChatRequest{Model: "codex", Raw: []byte(`{"messages":[],"reasoning":{"effort":"medium","summary":"detailed","encrypted_content":"secret"}}`)}
	got, err := ToResponsesRequest(req)
	if err != nil {
		t.Fatalf("ToResponsesRequest: %v", err)
	}
	want := map[string]any{"effort": "medium", "summary": "detailed"}
	if !reflect.DeepEqual(got["reasoning"], want) {
		t.Fatalf("reasoning = %#v, want %#v", got["reasoning"], want)
	}
}

func TestToResponsesRequestMapsSimpleResponseFormatsAndToolChoice(t *testing.T) {
	tests := []struct {
		name   string
		format string
		choice string
	}{
		{name: "text", format: "text", choice: "auto"},
		{name: "json", format: "json_object", choice: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{"messages":[],"response_format":{"type":"` + tt.format + `"},"tool_choice":"` + tt.choice + `"}`)
			got, err := ToResponsesRequest(provider.ChatRequest{Model: "codex", Raw: raw})
			if err != nil {
				t.Fatalf("ToResponsesRequest: %v", err)
			}
			wantText := map[string]any{"format": map[string]any{"type": tt.format}}
			if !reflect.DeepEqual(got["text"], wantText) || got["tool_choice"] != tt.choice {
				t.Fatalf("text = %#v, tool_choice = %#v", got["text"], got["tool_choice"])
			}
		})
	}
}

func TestToResponsesRequestUsesStrictAllowlist(t *testing.T) {
	req := provider.ChatRequest{Model: "codex", Raw: []byte(`{
		"messages":[],"temperature":1,"top_p":1,"max_tokens":20,"max_completion_tokens":30,
		"metadata":{"secret":"value"},"user":"person","service_tier":"priority","stream":false,"store":true
	}`)}
	got, err := ToResponsesRequest(req)
	if err != nil {
		t.Fatalf("ToResponsesRequest: %v", err)
	}
	allowed := map[string]bool{
		"model": true, "instructions": true, "input": true, "tools": true,
		"tool_choice": true, "parallel_tool_calls": true, "reasoning": true,
		"text": true, "store": true, "stream": true, "include": true,
	}
	for key := range got {
		if !allowed[key] {
			t.Fatalf("unsupported field %q forwarded in %#v", key, got)
		}
	}
	if got["store"] != false || got["stream"] != true {
		t.Fatalf("store/stream = %#v/%#v", got["store"], got["stream"])
	}
}

func TestToResponsesRequestRejectsMalformedJSONAndTypes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "json", raw: `{"messages":`},
		{name: "root", raw: `[]`},
		{name: "messages", raw: `{"messages":{}}`},
		{name: "role", raw: `{"messages":[{"role":1,"content":"x"}]}`},
		{name: "content", raw: `{"messages":[{"role":"user","content":1}]}`},
		{name: "image", raw: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":1}}]}]}`},
		{name: "tools", raw: `{"messages":[],"tools":"bad"}`},
		{name: "reasoning", raw: `{"messages":[],"reasoning_effort":1}`},
		{name: "format", raw: `{"messages":[],"response_format":{"type":"xml"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ToResponsesRequest(provider.ChatRequest{Model: "codex", Raw: []byte(tt.raw)})
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, errInvalidTranslation) && tt.name != "json" {
				t.Fatalf("error %v does not wrap errInvalidTranslation", err)
			}
		})
	}
}

func TestParseResponseEventRejectsMalformedPayload(t *testing.T) {
	tests := [][]byte{
		[]byte(`data: {`),
		[]byte(`{"type":1}`),
		[]byte(`{"type":"response.output_text.delta","delta":1}`),
		[]byte(`{"type":"response.created","response":{"id":1}}`),
	}
	for _, payload := range tests {
		if _, err := parseResponseEvent(payload); err == nil {
			t.Fatalf("parseResponseEvent(%q) succeeded", payload)
		}
	}
}
