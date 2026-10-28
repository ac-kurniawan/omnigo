package provider

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestResponsesRequestTranslatesInstructionsAndStringInput(t *testing.T) {
	req, err := ResponsesRequest([]byte(`{"model":"m","instructions":"be brief","input":"hi"}`))
	if err != nil {
		t.Fatalf("ResponsesRequest: %v", err)
	}
	want := []Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"}}
	if !reflect.DeepEqual(req.Messages, want) {
		t.Fatalf("messages = %#v, want %#v", req.Messages, want)
	}
	if req.Model != "m" || req.Stream {
		t.Fatalf("model/stream = %q/%v", req.Model, req.Stream)
	}
	if _, ok := req.Parsed["max_tokens"]; ok {
		t.Fatalf("max_tokens present without max_output_tokens: %#v", req.Parsed)
	}
	for _, key := range []string{"input", "instructions", "max_output_tokens", "text", "reasoning", "parallel_tool_calls"} {
		if _, ok := req.Parsed[key]; ok {
			t.Fatalf("responses-only key %q leaked into the chat payload", key)
		}
	}

	req, err = ResponsesRequest([]byte(`{"model":"m","input":"hi","max_output_tokens":64,"stream":true}`))
	if err != nil {
		t.Fatalf("ResponsesRequest: %v", err)
	}
	if req.Parsed["max_tokens"] == nil || !req.Stream {
		t.Fatalf("max_tokens/stream = %#v/%v", req.Parsed["max_tokens"], req.Stream)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("messages = %#v", req.Messages)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(req.Raw, &forwarded); err != nil {
		t.Fatalf("Raw: %v", err)
	}
	if _, ok := forwarded["messages"]; !ok {
		t.Fatalf("Raw has no messages: %s", req.Raw)
	}
}

func TestResponsesRequestTranslatesItemList(t *testing.T) {
	body := `{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"https://x/y.png","detail":"high"}]},
		{"type":"function_call_output","call_id":"c1","output":{"ok":true}},
		{"role":"assistant","content":"previous answer"}
	]}`
	req, err := ResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("ResponsesRequest: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %#v", req.Messages)
	}
	parts, ok := req.Messages[0].Content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v", req.Messages[0].Content)
	}
	if parts[0].(map[string]any)["type"] != "text" || parts[0].(map[string]any)["text"] != "describe" {
		t.Fatalf("text part = %#v", parts[0])
	}
	image := parts[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("image part = %#v", image)
	}
	if url := image["image_url"].(map[string]any)["url"]; url != "https://x/y.png" {
		t.Fatalf("image url = %v", url)
	}
	if req.Messages[1].Role != "tool" || req.Messages[1].Content != `{"ok":true}` {
		t.Fatalf("tool message = %#v", req.Messages[1])
	}
	if req.Messages[2].Role != "assistant" || req.Messages[2].Content != "previous answer" {
		t.Fatalf("assistant message = %#v", req.Messages[2])
	}
}

func TestResponsesRequestTranslatesTopLevelTextItems(t *testing.T) {
	req, err := ResponsesRequest([]byte(`{"model":"m","input":[{"type":"input_text","text":"a"},{"type":"output_text","text":"b"}]}`))
	if err != nil {
		t.Fatalf("ResponsesRequest: %v", err)
	}
	want := []Message{{Role: "user", Content: "ab"}}
	if !reflect.DeepEqual(req.Messages, want) {
		t.Fatalf("messages = %#v, want %#v", req.Messages, want)
	}
}

func TestResponsesRequestRejectsStatefulAndFileInputs(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"previous_response_id", `{"model":"m","input":"hi","previous_response_id":"resp_1"}`, "responses: previous_response_id is not supported by this gateway"},
		{"conversation", `{"model":"m","input":"hi","conversation":"conv_1"}`, "responses: conversation is not supported by this gateway"},
		{"prompt", `{"model":"m","input":"hi","prompt":{"id":"p"}}`, "responses: prompt is not supported by this gateway"},
		{"background", `{"model":"m","input":"hi","background":true}`, "responses: background is not supported by this gateway"},
		{"include", `{"model":"m","input":"hi","include":["a"]}`, "responses: include is not supported by this gateway"},
		{"store", `{"model":"m","input":"hi","store":true}`, "responses: store is not supported by this gateway"},
		{"input_file", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"f"}]}]}`, "responses: input[0] file inputs are not supported"},
		{"file_id", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","file_id":"f"}]}]}`, "responses: input[0] file inputs are not supported"},
		{"item type", `{"model":"m","input":[{"type":"function_call","name":"f"}]}`, `responses: input[0].type "function_call" is not supported`},
		{"role", `{"model":"m","input":[{"type":"message","role":"tool","content":"x"}]}`, `responses: input[0].role "tool" is not supported`},
		{"tool type", `{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`, `responses: tool type "web_search" is not supported`},
		{"missing model", `{"input":"hi"}`, "responses: model is required"},
		{"invalid json", `{`, "responses: invalid JSON body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResponsesRequest([]byte(tc.body))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestResponsesRequestAcceptsFalseStore(t *testing.T) {
	if _, err := ResponsesRequest([]byte(`{"model":"m","input":"hi","store":false}`)); err != nil {
		t.Fatalf("store:false must be accepted: %v", err)
	}
}

func TestResponsesRequestTranslatesFunctionTools(t *testing.T) {
	body := `{"model":"m","input":"hi",
		"tools":[{"type":"function","name":"get_weather","description":"d","parameters":{"type":"object"},"strict":true}],
		"tool_choice":{"type":"function","name":"get_weather"}}`
	req, err := ResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("ResponsesRequest: %v", err)
	}
	tools, ok := req.Parsed["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", req.Parsed["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Fatalf("tool type = %v", tool["type"])
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok || fn["name"] != "get_weather" || fn["description"] != "d" || fn["strict"] != true {
		t.Fatalf("function = %#v", tool["function"])
	}
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Fatalf("parameters = %#v", fn["parameters"])
	}
	choice, ok := req.Parsed["tool_choice"].(map[string]any)
	if !ok || choice["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tool_choice = %#v", req.Parsed["tool_choice"])
	}
}

func TestResponsesRequestOmitsAbsentTools(t *testing.T) {
	req, err := ResponsesRequest([]byte(`{"model":"m","input":"hi","tool_choice":"auto"}`))
	if err != nil {
		t.Fatalf("ResponsesRequest: %v", err)
	}
	if _, ok := req.Parsed["tools"]; ok {
		t.Fatalf("tools key present with no tools: %#v", req.Parsed)
	}
	if req.Parsed["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %#v", req.Parsed["tool_choice"])
	}
}
