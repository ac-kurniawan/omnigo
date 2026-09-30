package codex

import (
	"reflect"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func applyProfileForTest(t *testing.T, req provider.ChatRequest, model string) map[string]any {
	t.Helper()
	got, err := ToResponsesRequest(req)
	if err != nil {
		t.Fatalf("ToResponsesRequest: %v", err)
	}
	profile, ok := builtinProfiles[model]
	if !ok {
		t.Fatalf("no builtin profile for %s", model)
	}
	applyModelProfile(got, profile)
	return got
}

func TestApplyModelProfileLiteMovesInstructionsAndToolsIntoInput(t *testing.T) {
	req := provider.ChatRequest{Model: "gpt-6-sol", Raw: []byte(`{
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"hi"}
		],
		"tools":[{"type":"function","function":{
			"name":"weather","description":"Get weather",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}
		}}]
	}`)}

	got := applyProfileForTest(t, req, "gpt-6-sol")
	input := got["input"].([]any)
	want := []any{
		map[string]any{
			"type": "additional_tools", "role": "developer",
			"tools": []any{map[string]any{
				"type": "namespace", "name": "functions", "description": "",
				"tools": []any{map[string]any{
					"type": "function", "name": "weather", "description": "Get weather",
					"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
				}},
			}},
		},
		map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "be brief"}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
	}
	if !reflect.DeepEqual(input, want) {
		t.Fatalf("input = %#v\nwant %#v", input, want)
	}
	if _, exists := got["instructions"]; exists {
		t.Fatalf("instructions key must be deleted, got %#v", got["instructions"])
	}
	if _, exists := got["tools"]; exists {
		t.Fatalf("tools key must be deleted, got %#v", got["tools"])
	}
	if got["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v, want false", got["parallel_tool_calls"])
	}
	if !reflect.DeepEqual(got["reasoning"], map[string]any{"effort": "medium", "context": "all_turns"}) {
		t.Fatalf("reasoning = %#v", got["reasoning"])
	}
}

func TestApplyModelProfileLiteWithoutInstructionsOrTools(t *testing.T) {
	req := provider.ChatRequest{Model: "gpt-6-luna", Raw: []byte(`{
		"messages":[{"role":"user","content":"hi"}]
	}`)}

	got := applyProfileForTest(t, req, "gpt-6-luna")
	input := got["input"].([]any)
	want := []any{
		map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
	}
	if !reflect.DeepEqual(input, want) {
		t.Fatalf("input = %#v\nwant %#v", input, want)
	}
	reasoning := got["reasoning"].(map[string]any)
	if reasoning["context"] != "all_turns" {
		t.Fatalf("reasoning.context = %#v, want all_turns", reasoning["context"])
	}
}

func TestApplyModelProfileNormalisesEffort(t *testing.T) {
	testcases := []struct {
		model  string
		effort string
		want   string
	}{
		{"gpt-6-luna", "none", "low"},
		{"gpt-6-luna", "ultra", "max"},
		{"gpt-6-sol", "minimal", "low"},
		{"gpt-6-sol", "ultra", "ultra"},
		{"gpt-5.5", "max", "xhigh"},
		{"gpt-5.5", "high", "high"},
	}
	for _, tc := range testcases {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			req := provider.ChatRequest{Model: tc.model, Raw: []byte(`{
				"messages":[{"role":"user","content":"hi"}],
				"reasoning_effort":"` + tc.effort + `"
			}`)}
			got := applyProfileForTest(t, req, tc.model)
			reasoning := got["reasoning"].(map[string]any)
			if reasoning["effort"] != tc.want {
				t.Fatalf("effort = %#v, want %q", reasoning["effort"], tc.want)
			}
			if tc.model == "gpt-5.5" && tc.effort == "high" {
				untouched, err := ToResponsesRequest(req)
				if err != nil {
					t.Fatalf("ToResponsesRequest: %v", err)
				}
				if !reflect.DeepEqual(got, untouched) {
					t.Fatalf("body must stay byte-identical for a valid non-Lite effort:\ngot %#v\nwant %#v", got, untouched)
				}
			}
		})
	}
}

func TestApplyModelProfileLiteKeepsToolReplayOrder(t *testing.T) {
	req := provider.ChatRequest{Model: "gpt-6-sol", Raw: []byte(`{
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"weather"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"{\"temp\":20}"}
		]
	}`)}

	got := applyProfileForTest(t, req, "gpt-6-sol")
	input := got["input"].([]any)
	if len(input) != 5 {
		t.Fatalf("input length = %d, want 5: %#v", len(input), input)
	}
	if input[0].(map[string]any)["type"] != "additional_tools" {
		t.Fatalf("input[0] = %#v, want additional_tools", input[0])
	}
	if input[1].(map[string]any)["role"] != "developer" {
		t.Fatalf("input[1] = %#v, want developer message", input[1])
	}
	wantTail := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "weather"}}},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "weather", "arguments": `{"city":"Paris"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": `{"temp":20}`},
	}
	if !reflect.DeepEqual(input[2:], wantTail) {
		t.Fatalf("input[2:] = %#v\nwant %#v", input[2:], wantTail)
	}
	if _, exists := input[3].(map[string]any)["namespace"]; exists {
		t.Fatalf("function_call must not carry a namespace key: %#v", input[3])
	}
}

func TestApplyModelProfileLiteStripsImageDetail(t *testing.T) {
	req := provider.ChatRequest{Model: "gpt-6-sol", Raw: []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image_url","image_url":{"url":"https://example.invalid/a.png","detail":"high"}}
		]}]
	}`)}

	got := applyProfileForTest(t, req, "gpt-6-sol")
	input := got["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input length = %d, want 2 (no instructions): %#v", len(input), input)
	}
	user := input[1].(map[string]any)["content"].([]any)
	want := []any{
		map[string]any{"type": "input_text", "text": "look"},
		map[string]any{"type": "input_image", "image_url": "https://example.invalid/a.png"},
	}
	if !reflect.DeepEqual(user, want) {
		t.Fatalf("user content = %#v\nwant %#v", user, want)
	}
}

func TestApplyModelProfileLiteKeepsExplicitSummary(t *testing.T) {
	req := provider.ChatRequest{Model: "gpt-6-sol", Raw: []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"reasoning":{"effort":"high","summary":"detailed"}
	}`)}

	got := applyProfileForTest(t, req, "gpt-6-sol")
	want := map[string]any{"effort": "high", "summary": "detailed", "context": "all_turns"}
	if !reflect.DeepEqual(got["reasoning"], want) {
		t.Fatalf("reasoning = %#v, want %#v", got["reasoning"], want)
	}
}
