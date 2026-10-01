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

func TestDeclarationSchemaCollapsesUnionType(t *testing.T) {
	got := declarationSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skip":  map[string]any{"type": []any{"number", "null"}},
			"multi": map[string]any{"type": []any{"null", "string", "integer"}},
			"type":  map[string]any{"type": "string"},
		},
	})
	props := got["properties"].(map[string]any)
	skip := props["skip"].(map[string]any)
	if skip["type"] != "number" || skip["nullable"] != true {
		t.Fatalf("skip = %#v", skip)
	}
	multi := props["multi"].(map[string]any)
	if multi["type"] != "string" || multi["nullable"] != true {
		t.Fatalf("multi = %#v", multi)
	}
	if props["type"].(map[string]any)["type"] != "string" {
		t.Fatalf("property named type = %#v", props["type"])
	}
}

func TestToEnvelopePreservesSchemaPropertyNamesAndValidRequired(t *testing.T) {
	parsed := provider.ParseBody([]byte(`{"messages":[{"role":"user","content":"search"}],"tools":[
		{"type":"function","function":{"name":"one","parameters":{}}},
		{"type":"function","function":{"name":"two","parameters":{}}},
		{"type":"function","function":{"name":"three","parameters":{}}},
		{"type":"function","function":{"name":"four","parameters":{}}},
		{"type":"function","function":{"name":"five","parameters":{}}},
		{"type":"function","function":{"name":"six","parameters":{}}},
		{"type":"function","function":{"name":"search","parameters":{
			"type":"object","properties":{
				"pattern":{"type":"string"},"format":{"type":"string"},
				"safe":{"type":"string","format":"date"},
				"nested":{"type":"object","properties":{"strict":{"type":"boolean"}},"required":["strict","gone"]},
				"empty":{"type":"object","required":["orphan"]}
			},"required":["pattern","safe","missing"]
		}}}]}`))
	req := provider.ChatRequest{Model: "gemini", Parsed: parsed}
	env, err := ToEnvelope("proj", req.Model, req)
	if err != nil {
		t.Fatal(err)
	}
	decls := env["request"].(map[string]any)["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	if len(decls) != 7 {
		t.Fatalf("declarations = %d, want 7", len(decls))
	}
	schema := decls[6].(map[string]any)["parameters"].(map[string]any)
	props := schema["properties"].(map[string]any)
	for name, want := range map[string]string{"pattern": "string", "format": "string", "safe": "string"} {
		property, ok := props[name].(map[string]any)
		if !ok || property["type"] != want {
			t.Errorf("property %q = %#v, want type %q", name, props[name], want)
		}
	}
	if _, ok := props["safe"].(map[string]any)["format"]; ok {
		t.Errorf("unsupported schema keyword survived: %#v", props["safe"])
	}
	if got, _ := json.Marshal(schema["required"]); string(got) != `["pattern","safe"]` {
		t.Errorf("root required = %s", got)
	}
	nested := props["nested"].(map[string]any)
	strict, ok := nested["properties"].(map[string]any)["strict"].(map[string]any)
	if !ok || strict["type"] != "boolean" {
		t.Errorf("nested strict = %#v", nested["properties"])
	}
	if got, _ := json.Marshal(nested["required"]); string(got) != `["strict"]` {
		t.Errorf("nested required = %s", got)
	}
	if _, ok := props["empty"].(map[string]any)["required"]; ok {
		t.Errorf("empty object kept orphaned required: %#v", props["empty"])
	}
	original := parsed["tools"].([]any)[6].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if got, _ := json.Marshal(original["required"]); string(got) != `["pattern","safe","missing"]` {
		t.Errorf("source root required mutated: %s", got)
	}
	originalProps := original["properties"].(map[string]any)
	if got, _ := json.Marshal(originalProps["nested"].(map[string]any)["required"]); string(got) != `["strict","gone"]` {
		t.Errorf("source nested required mutated: %s", got)
	}
	if got, _ := json.Marshal(originalProps["empty"].(map[string]any)["required"]); string(got) != `["orphan"]` {
		t.Errorf("source empty required mutated: %s", got)
	}
	if originalProps["safe"].(map[string]any)["format"] != "date" {
		t.Errorf("source safe.format mutated: %#v", originalProps["safe"])
	}
}

func TestToEnvelopeDeclaresToolsAndMapsToolTurns(t *testing.T) {
	req := provider.ChatRequest{
		Model: "gemini-3.7-flash-medium",
		Parsed: provider.ParseBody([]byte(`{
			"model":"gemini-3.7-flash-medium",
			"messages":[
				{"role":"system","content":"be brief"},
				{"role":"user","content":"weather"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Rome\"}"}}
				]},
				{"role":"tool","tool_call_id":"call_1","content":"{\"temp\":20}"}
			],
			"tools":[{"type":"function","function":{
				"name":"weather","description":"Lookup weather",
				"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}
			}}],
			"tool_choice":"auto",
			"max_tokens":128
		}`)),
	}
	env, err := ToEnvelope("proj-1", req.Model, req)
	if err != nil {
		t.Fatalf("ToEnvelope: %v", err)
	}
	inner := env["request"].(map[string]any)
	tools := inner["tools"].([]any)
	decls := tools[0].(map[string]any)["functionDeclarations"].([]any)
	decl := decls[0].(map[string]any)
	if decl["name"] != "weather" {
		t.Fatalf("declaration = %#v", decl)
	}
	schema := decl["parameters"].(map[string]any)
	if _, ok := schema["additionalProperties"]; ok {
		t.Fatalf("schema kept additionalProperties: %#v", schema)
	}
	if inner["generationConfig"].(map[string]any)["maxOutputTokens"] != float64(128) {
		t.Fatalf("generationConfig = %#v", inner["generationConfig"])
	}
	if inner["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"] != "AUTO" {
		t.Fatalf("toolConfig = %#v", inner["toolConfig"])
	}
	if _, exists := env["requestType"]; exists {
		t.Fatalf("requestType leaked: %#v", env["requestType"])
	}
	contents := inner["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %#v", contents)
	}
	model := contents[1].(map[string]any)
	call := model["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	if model["role"] != "model" || call["name"] != "weather" || call["id"] != "call_1" {
		t.Fatalf("model turn = %#v", model)
	}
	if model["parts"].([]any)[0].(map[string]any)["thoughtSignature"] == nil {
		t.Fatalf("function call missing thought signature: %#v", model)
	}
	reply := contents[2].(map[string]any)
	got := reply["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if reply["role"] != "user" || got["name"] != "weather" || got["id"] != "call_1" {
		t.Fatalf("tool reply = %#v", reply)
	}
	sys := inner["systemInstruction"].(map[string]any)
	if sys["parts"].([]any)[0].(map[string]any)["text"] != "be brief" {
		t.Fatalf("system = %#v", sys)
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
