package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessagesRequestTranslatesSystemAndTurns(t *testing.T) {
	req, err := MessagesRequest([]byte(`{
		"model":"claude-sonnet",
		"max_tokens":1024,
		"stream":true,
		"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"text","text":"hello"}]}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "claude-sonnet" || !req.Stream {
		t.Fatalf("request = %+v", req)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "be brief" {
		t.Fatalf("system = %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "hi" {
		t.Fatalf("user = %+v", req.Messages[1])
	}
	if req.Messages[2].Role != "assistant" {
		t.Fatalf("assistant = %+v", req.Messages[2])
	}
	var body map[string]any
	if err := json.Unmarshal(req.Raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["max_tokens"] != float64(1024) {
		t.Fatalf("max_tokens = %v", body["max_tokens"])
	}
	if _, ok := body["system"]; ok {
		t.Fatalf("anthropic system field leaked upstream: %v", body["system"])
	}
	if _, ok := body["max_tokens"].(json.Number); !ok {
		// ParseBody uses UseNumber; either numeric form is fine, but the key must exist.
		if body["max_tokens"] == nil {
			t.Fatal("max_tokens missing")
		}
	}
}

func TestMessagesRequestFoldsSystemRoleTurn(t *testing.T) {
	req, err := MessagesRequest([]byte(`{"model":"m","max_tokens":8,
		"system":"base",
		"messages":[{"role":"system","content":"reminder"},{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Content != "base\nreminder" {
		t.Fatalf("system = %q", req.Messages[0].Content)
	}
	for _, msg := range req.Messages[1:] {
		if msg.Role == "system" {
			t.Fatalf("system turn left in messages: %+v", req.Messages)
		}
	}
}

func TestMessagesRequestTranslatesToolsAndToolUse(t *testing.T) {
	req, err := MessagesRequest([]byte(`{
		"model":"m","max_tokens":8,
		"tools":[{"name":"get","description":"get","input_schema":{"type":"object"},"strict":true}],
		"tool_choice":{"type":"tool","name":"get"},
		"messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"plan","signature":"sig"},
				{"type":"tool_use","id":"tu_1","name":"get","input":{"q":"x"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"42"}]}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	tool := req.Messages[2]
	if tool.Role != "tool" || tool.Content != "42" {
		t.Fatalf("tool result = %+v", tool)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Raw, &body); err != nil {
		t.Fatal(err)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", body["tools"])
	}
	tool0 := tools[0].(map[string]any)
	fn := tool0["function"].(map[string]any)
	if fn["name"] != "get" || fn["parameters"] == nil {
		t.Fatalf("tool = %v", tool0)
	}
	if _, ok := fn["strict"]; ok {
		t.Fatalf("strict leaked: %v", fn)
	}
	choice := body["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["function"].(map[string]any)["name"] != "get" {
		t.Fatalf("tool_choice = %v", choice)
	}
	for _, key := range []string{"thinking", "metadata", "cache_control"} {
		if _, ok := body[key]; ok {
			t.Fatalf("%s leaked upstream", key)
		}
	}
	// The thinking block must not be echoed back; the tool_use must be, with
	// its id so the following tool_result can correlate.
	wire, err := req.Body()
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(wire, &chat); err != nil {
		t.Fatal(err)
	}
	var assistant map[string]any
	for _, turn := range chat.Messages {
		if turn["role"] == "assistant" {
			assistant = turn
		}
	}
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "tu_1" {
		t.Fatalf("tool_calls = %+v", assistant)
	}
	if strings.Contains(string(req.Raw), "thinking") {
		t.Fatalf("thinking leaked into raw body: %s", req.Raw)
	}
}

func TestMessagesRequestTranslatesToolChoiceModes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"type":"auto"}`, "auto"},
		{`{"type":"any"}`, "required"},
		{`{"type":"none"}`, "none"},
	} {
		req, err := MessagesRequest([]byte(`{"model":"m","max_tokens":8,"tool_choice":` + tc.in + `,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		var body map[string]any
		_ = json.Unmarshal(req.Raw, &body)
		if body["tool_choice"] != tc.want {
			t.Fatalf("%s: tool_choice = %v, want %s", tc.in, body["tool_choice"], tc.want)
		}
	}
}

func TestMessagesRequestMapsStopSequencesAndImage(t *testing.T) {
	req, err := MessagesRequest([]byte(`{
		"model":"m","max_tokens":8,"stop_sequences":["END"],
		"messages":[{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
		]}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(req.Raw, &body)
	stop, _ := body["stop"].([]any)
	if len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop = %v", body["stop"])
	}
	var rid struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(req.Raw, &rid)
	parts, _ := rid.Messages[0]["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %+v", parts)
	}
	image := parts[1].(map[string]any)
	url := image["image_url"].(map[string]any)["url"].(string)
	if url != "data:image/png;base64,AAAA" {
		t.Fatalf("image url = %q", url)
	}
}

// A tool turn must carry the upstream tool_call_id, and an assistant turn that
// both speaks and calls a tool must keep its text.
func TestMessagesRequestPreservesToolCorrelationAndAssistantText(t *testing.T) {
	req, err := MessagesRequest([]byte(`{
		"model":"m","max_tokens":8,
		"messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","content":[
				{"type":"text","text":"I will inspect the file"},
				{"type":"tool_use","id":"tu_1","name":"get","input":{"q":"x"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"42"}]}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(req.Raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %+v", body.Messages)
	}
	assistant := body.Messages[1]
	if assistant["content"] != "I will inspect the file" {
		t.Fatalf("assistant text dropped: %+v", assistant)
	}
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "tu_1" {
		t.Fatalf("assistant tool_calls = %+v", assistant["tool_calls"])
	}
	tool := body.Messages[2]
	if tool["role"] != "tool" || tool["tool_call_id"] != "tu_1" || tool["content"] != "42" {
		t.Fatalf("tool message = %+v", tool)
	}
}

func TestMessagesRequestSplitsMixedUserTurnAroundToolResult(t *testing.T) {
	req, err := MessagesRequest([]byte(`{
		"model":"m","max_tokens":8,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"get","input":{}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":"42"},
				{"type":"text","text":"thanks"}
			]}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(req.Raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %+v", body.Messages)
	}
	if body.Messages[1]["role"] != "tool" || body.Messages[1]["tool_call_id"] != "tu_1" {
		t.Fatalf("tool message = %+v", body.Messages[1])
	}
	if body.Messages[2]["role"] != "user" || body.Messages[2]["content"] != "thanks" {
		t.Fatalf("trailing text = %+v", body.Messages[2])
	}
}

func TestMessagesRequestRejectsInvalidShapes(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"max_tokens null", `{"model":"m","max_tokens":null,"messages":[{"role":"user","content":"hi"}]}`},
		{"max_tokens string", `{"model":"m","max_tokens":"8","messages":[{"role":"user","content":"hi"}]}`},
		{"max_tokens zero", `{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`},
		{"max_tokens negative", `{"model":"m","max_tokens":-1,"messages":[{"role":"user","content":"hi"}]}`},
		{"max_tokens fractional", `{"model":"m","max_tokens":1.5,"messages":[{"role":"user","content":"hi"}]}`},
		{"missing messages", `{"model":"m","max_tokens":8}`},
		{"empty messages", `{"model":"m","max_tokens":8,"messages":[]}`},
		{"messages not array", `{"model":"m","max_tokens":8,"messages":"hi"}`},
		{"bad role", `{"model":"m","max_tokens":8,"messages":[{"role":"robot","content":"hi"}]}`},
		{"content wrong type", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":7}]}`},
		{"unknown block", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"document"}]}]}`},
		{"tool_use without id", `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"get","input":{}}]}]}`},
		{"stream not bool", `{"model":"m","max_tokens":8,"stream":"yes","messages":[{"role":"user","content":"hi"}]}`},
		{"server tool", `{"model":"m","max_tokens":8,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"hi"}]}`},
		{"bad tool_choice", `{"model":"m","max_tokens":8,"tool_choice":{"type":"weird"},"messages":[{"role":"user","content":"hi"}]}`},
	} {
		if _, err := MessagesRequest([]byte(tc.body)); err == nil {
			t.Errorf("%s: accepted invalid body", tc.name)
		}
	}
}

// A server tool has no chat equivalent; rejecting it beats silently telling
// the model the tool vanished.
// A tool result can carry an image or other non-text block. Dropping it would
// lose the payload the model needs on the next turn.
func TestMessagesRequestKeepsNonTextToolResult(t *testing.T) {
	req, err := MessagesRequest([]byte(`{
		"model":"m","max_tokens":8,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"get","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":[
				{"type":"text","text":"see"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
			]}]}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(req.Raw, &body); err != nil {
		t.Fatal(err)
	}
	tool := body.Messages[1]
	content, _ := tool["content"].([]any)
	if tool["tool_call_id"] != "tu_1" || len(content) != 2 {
		t.Fatalf("tool result = %+v", tool)
	}
	image := content[1].(map[string]any)
	if image["type"] != "image_url" || image["image_url"].(map[string]any)["url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("image part = %+v", image)
	}
}

func TestMessagesRequestRejectsServerTools(t *testing.T) {
	_, err := MessagesRequest([]byte(`{"model":"m","max_tokens":8,
		"tools":[{"type":"web_search_20250305","name":"web_search"}],
		"messages":[{"role":"user","content":"hi"}]}`))
	if err == nil {
		t.Fatal("accepted a server tool")
	}
}

func TestMessagesRequestRequiresModelAndMaxTokens(t *testing.T) {
	if _, err := MessagesRequest([]byte(`{"max_tokens":8,"messages":[]}`)); err == nil {
		t.Fatal("expected missing model error")
	}
	if _, err := MessagesRequest([]byte(`{"model":"m","messages":[]}`)); err == nil {
		t.Fatal("expected missing max_tokens error")
	}
	if _, err := MessagesRequest([]byte(`not json`)); err == nil {
		t.Fatal("expected invalid JSON error")
	}
}
