package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

// responsesOmittedKeys are Responses-only fields that have no Chat Completions
// meaning. They are dropped from the rebuilt payload rather than forwarded,
// because an upstream chat endpoint rejects unknown top-level fields.
var responsesOmittedKeys = []string{
	"input", "instructions", "max_output_tokens", "text", "reasoning",
	"parallel_tool_calls",
}

// ResponsesRequest translates a stateless Responses API body into the
// ChatRequest the combo engine and providers already accept. raw is retained
// so forwarding providers send the client's original bytes.
func ResponsesRequest(raw []byte) (ChatRequest, error) {
	body := ParseBody(raw)
	if body == nil {
		return ChatRequest{}, fmt.Errorf("responses: invalid JSON body")
	}

	for _, key := range []string{"previous_response_id", "conversation", "prompt", "background", "include"} {
		if v, ok := body[key]; ok && v != nil {
			return ChatRequest{}, fmt.Errorf("responses: %s is not supported by this gateway", key)
		}
	}
	if store, ok := body["store"].(bool); ok && store {
		return ChatRequest{}, fmt.Errorf("responses: store is not supported by this gateway")
	}

	model, _ := body["model"].(string)
	if model == "" {
		return ChatRequest{}, fmt.Errorf("responses: model is required")
	}

	var messages []Message
	if instructions := instructionText(body["instructions"]); instructions != "" {
		messages = append(messages, Message{Role: "system", Content: instructions})
	}
	input, err := responsesInput(body["input"])
	if err != nil {
		return ChatRequest{}, err
	}
	messages = append(messages, input...)

	tools, err := responsesTools(body["tools"])
	if err != nil {
		return ChatRequest{}, err
	}
	toolChoice, err := responsesToolChoice(body["tool_choice"])
	if err != nil {
		return ChatRequest{}, err
	}

	stream, _ := body["stream"].(bool)
	parsed := make(map[string]any, len(body))
	for k, v := range body {
		if k == openAIBodyCacheKey {
			continue
		}
		parsed[k] = v
	}
	for _, key := range responsesOmittedKeys {
		delete(parsed, key)
	}
	parsed["messages"] = messages
	if maxTokens, ok := body["max_output_tokens"]; ok && maxTokens != nil {
		parsed["max_tokens"] = maxTokens
	}
	if len(tools) > 0 {
		parsed["tools"] = tools
	}
	if toolChoice != nil {
		parsed["tool_choice"] = toolChoice
	}

	encoded, err := json.Marshal(parsed)
	if err != nil {
		return ChatRequest{}, fmt.Errorf("responses: invalid JSON body")
	}
	return ChatRequest{
		Model:    model,
		Stream:   stream,
		Messages: messages,
		Raw:      encoded,
		Parsed:   parsed,
	}, nil
}

// instructionText renders the Responses "instructions" field: a plain string,
// or an array of text parts joined with newlines. Absent or empty is "".
func instructionText(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

// responsesInput converts the Responses "input" field into chat messages.
func responsesInput(raw any) ([]Message, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case string:
		return []Message{{Role: "user", Content: v}}, nil
	case []any:
		return responsesInputItems(v)
	}
	return nil, fmt.Errorf("responses: input is not supported")
}

// responsesInputItems walks an item list. Message items become chat messages;
// loose input_text/output_text items fold into one synthetic user message;
// function_call_output becomes a tool message. Server-side items (function_call,
// reasoning, web_search_call, ...) cannot be replayed faithfully and are
// rejected rather than silently dropped.
func responsesInputItems(items []any) ([]Message, error) {
	messages := make([]Message, 0, len(items))
	var pending []string

	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		content := strings.Join(pending, "")
		messages = append(messages, Message{Role: "user", Content: content})
		pending = pending[:0]
	}

	for i, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("responses: input[%d] is not supported", i)
		}
		kind, _ := item["type"].(string)
		role, _ := item["role"].(string)

		switch {
		case kind == "" && role != "":
			kind = "message"
		case kind == "input_text" || kind == "output_text":
			if text, ok := item["text"].(string); ok {
				pending = append(pending, text)
			}
			continue
		case kind == "function_call_output":
			flushPending()
			messages = append(messages, Message{Role: "tool", Content: toolOutputText(item["output"])})
			continue
		}

		if kind != "message" {
			return nil, fmt.Errorf("responses: input[%d].type %q is not supported", i, kind)
		}
		if role == "" {
			role = "user"
		}
		switch role {
		case "user", "assistant", "system", "developer":
		default:
			return nil, fmt.Errorf("responses: input[%d].role %q is not supported", i, role)
		}
		content, err := responsesContent(i, item["content"])
		if err != nil {
			return nil, err
		}
		flushPending()
		messages = append(messages, Message{Role: role, Content: content})
	}
	flushPending()
	return messages, nil
}

// responsesContent translates one message's content: a string passes through,
// a part list becomes a Chat Completions part list.
func responsesContent(index int, raw any) (any, error) {
	switch v := raw.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case []any:
		parts := make([]any, 0, len(v))
		for _, part := range v {
			translated, err := responsesPart(index, part)
			if err != nil {
				return nil, err
			}
			if translated != nil {
				parts = append(parts, translated)
			}
		}
		return parts, nil
	}
	return nil, fmt.Errorf("responses: input[%d].content is not supported", index)
}

func responsesPart(index int, raw any) (any, error) {
	part, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("responses: input[%d].content part is not supported", index)
	}
	switch part["type"] {
	case "input_text", "output_text":
		text, _ := part["text"].(string)
		return map[string]any{"type": "text", "text": text}, nil
	case "input_image":
		if _, ok := part["file_id"].(string); ok {
			return nil, fmt.Errorf("responses: input[%d] file inputs are not supported", index)
		}
		image := map[string]any{}
		if url, ok := part["image_url"].(string); ok {
			image["url"] = url
		}
		if detail, ok := part["detail"].(string); ok && detail != "" {
			image["detail"] = detail
		}
		return map[string]any{"type": "image_url", "image_url": image}, nil
	case "input_file":
		return nil, fmt.Errorf("responses: input[%d] file inputs are not supported", index)
	}
	return nil, fmt.Errorf("responses: input[%d].content part type %q is not supported", index, part["type"])
}

// toolOutputText renders a function_call_output payload. Chat tool messages
// carry a string, so a structured output is encoded instead of dropped.
func toolOutputText(raw any) string {
	if text, ok := raw.(string); ok {
		return text
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// responsesTools converts Responses function tools to the Chat Completions
// shape. A built-in tool (web_search, file_search, ...) has no chat equivalent.
func responsesTools(raw any) ([]any, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, nil
	}
	tools := make([]any, 0, len(list))
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("responses: tool is not supported")
		}
		if kind, _ := tool["type"].(string); kind != "function" {
			return nil, fmt.Errorf("responses: tool type %q is not supported", kind)
		}
		fn := map[string]any{}
		for _, key := range []string{"name", "description", "parameters", "strict"} {
			if value, ok := tool[key]; ok {
				fn[key] = value
			}
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	return tools, nil
}

// responsesToolChoice translates the Responses tool_choice: a mode string
// passes through, a named function becomes the Chat Completions object.
func responsesToolChoice(raw any) (any, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case string:
		return v, nil
	case map[string]any:
		if kind, _ := v["type"].(string); kind != "function" {
			return nil, fmt.Errorf("responses: tool_choice type %q is not supported", kind)
		}
		name, _ := v["name"].(string)
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}, nil
	}
	return nil, fmt.Errorf("responses: tool_choice is not supported")
}
