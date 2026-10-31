package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

// messagesOmittedKeys are Anthropic-only top-level fields. No upstream this
// gateway talks to is Anthropic-shaped, and chat endpoints reject unknown
// fields, so they are dropped from the rebuilt payload (same rule as
// responsesOmittedKeys).
var messagesOmittedKeys = []string{
	"system", "stop_sequences", "thinking", "output_config", "context_management",
	"cache_control", "metadata", "service_tier", "speed", "container", "safeguards",
	"tools", "tool_choice",
}

// MessagesRequest translates an Anthropic Messages API body into the
// ChatRequest the combo engine and providers accept. Translation is lossy by
// design: thinking blocks are never echoed back, because their signatures are
// bound to the exact upstream request and Claude Code rejects a replay whose
// request was rewritten.
//
// Messages are translated straight into the chat wire turns rather than into
// provider.Message, because a chat turn carries fields Message does not
// (tool_calls, tool_call_id) and dropping them breaks tool round trips.
func MessagesRequest(raw []byte) (ChatRequest, error) {
	body := ParseBody(raw)
	if body == nil {
		return ChatRequest{}, fmt.Errorf("messages: invalid JSON body")
	}
	model, _ := body["model"].(string)
	if model == "" {
		return ChatRequest{}, fmt.Errorf("messages: model is required")
	}
	if err := messagesMaxTokens(body["max_tokens"]); err != nil {
		return ChatRequest{}, err
	}
	stream, hasStream := body["stream"].(bool)
	if _, present := body["stream"]; present && !hasStream {
		return ChatRequest{}, fmt.Errorf("messages: stream must be a boolean")
	}

	system := messagesSystemText(body["system"])
	turns, err := messagesTurns(body["messages"])
	if err != nil {
		return ChatRequest{}, err
	}
	// Claude Code emits system turns inside messages for unrecognized model
	// ids; chat upstreams only accept system first, so fold them there.
	rest := make([]any, 0, len(turns))
	for _, turn := range turns {
		if turn["role"] == "system" {
			system = joinText(system, turn["content"])
			continue
		}
		rest = append(rest, turn)
	}
	if system != "" {
		rest = append([]any{map[string]any{"role": "system", "content": system}}, rest...)
	}

	tools, err := messagesTools(body["tools"])
	if err != nil {
		return ChatRequest{}, err
	}
	choice, err := messagesToolChoice(body["tool_choice"])
	if err != nil {
		return ChatRequest{}, err
	}

	parsed := make(map[string]any, len(body))
	for k, v := range body {
		if k == openAIBodyCacheKey {
			continue
		}
		parsed[k] = v
	}
	for _, key := range messagesOmittedKeys {
		delete(parsed, key)
	}
	parsed["messages"] = rest
	if stop, ok := body["stop_sequences"].([]any); ok && len(stop) > 0 {
		parsed["stop"] = stop
	}
	if len(tools) > 0 {
		parsed["tools"] = tools
	}
	if choice != nil {
		parsed["tool_choice"] = choice
	}

	encoded, err := json.Marshal(parsed)
	if err != nil {
		return ChatRequest{}, fmt.Errorf("messages: invalid JSON body")
	}
	return ChatRequest{Model: model, Stream: stream, Messages: messagesFromTurns(rest), Raw: encoded, Parsed: parsed}, nil
}

// messagesFromTurns keeps req.Messages meaningful for callers that read it,
// while Raw and Parsed carry the complete wire turns.
func messagesFromTurns(turns []any) []Message {
	messages := make([]Message, 0, len(turns))
	for _, turn := range turns {
		if m, ok := turn.(map[string]any); ok {
			role, _ := m["role"].(string)
			messages = append(messages, Message{Role: role, Content: m["content"]})
		}
	}
	return messages
}

func messagesMaxTokens(raw any) error {
	if raw == nil {
		return fmt.Errorf("messages: max_tokens is required")
	}
	number, ok := raw.(json.Number)
	if !ok {
		return fmt.Errorf("messages: max_tokens must be a positive integer")
	}
	value, err := number.Int64()
	if err != nil || value <= 0 {
		return fmt.Errorf("messages: max_tokens must be a positive integer")
	}
	return nil
}

func messagesSystemText(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if part, ok := item.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

func joinText(a string, b any) string {
	text, _ := b.(string)
	switch {
	case a == "":
		return text
	case text == "":
		return a
	}
	return a + "\n" + text
}

// messagesTurns walks the Anthropic turns into chat wire turns. A tool_result
// becomes a tool message carrying its tool_call_id; text and images around it
// stay in their own turn so nothing is silently discarded.
func messagesTurns(raw any) ([]map[string]any, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("messages: messages must be an array")
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("messages: messages must not be empty")
	}
	var turns []map[string]any
	for i, item := range list {
		turn, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages: messages[%d] is not an object", i)
		}
		role, _ := turn["role"].(string)
		switch role {
		case "user", "assistant", "system", "developer":
		default:
			return nil, fmt.Errorf("messages: messages[%d].role %q is unsupported", i, role)
		}
		if text, ok := turn["content"].(string); ok {
			turns = append(turns, map[string]any{"role": role, "content": text})
			continue
		}
		blocks, ok := turn["content"].([]any)
		if !ok {
			return nil, fmt.Errorf("messages: messages[%d].content must be a string or an array", i)
		}
		translated, err := messagesContent(i, blocks, role)
		if err != nil {
			return nil, err
		}
		turns = append(turns, translated...)
	}
	if len(turns) == 0 {
		return nil, fmt.Errorf("messages: messages must not be empty")
	}
	return turns, nil
}

// messagesContent translates one turn's blocks. A tool_result flushes the
// pending text/image parts first, so ordering is preserved.
func messagesContent(index int, blocks []any, role string) ([]map[string]any, error) {
	var turns []map[string]any
	var parts []any
	var calls []any

	flush := func() {
		if len(parts) == 0 && len(calls) == 0 {
			return
		}
		wire := map[string]any{"role": role, "content": messagesParts(parts)}
		if len(calls) > 0 {
			wire["tool_calls"] = calls
		}
		turns = append(turns, wire)
		parts, calls = nil, nil
	}

	for j, block := range blocks {
		b, ok := block.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages: messages[%d].content[%d] is not an object", index, j)
		}
		switch b["type"] {
		case "text":
			text, ok := b["text"].(string)
			if !ok {
				return nil, fmt.Errorf("messages: messages[%d].content[%d].text must be a string", index, j)
			}
			if text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
		case "image":
			part, err := messagesImage(index, j, b["source"])
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case "tool_use":
			if role != "assistant" {
				return nil, fmt.Errorf("messages: messages[%d].content[%d] tool_use outside an assistant turn", index, j)
			}
			id, _ := b["id"].(string)
			name, _ := b["name"].(string)
			if id == "" || name == "" {
				return nil, fmt.Errorf("messages: messages[%d].content[%d] tool_use needs an id and a name", index, j)
			}
			args := "{}"
			if input, ok := b["input"]; ok && input != nil {
				encoded, err := json.Marshal(input)
				if err != nil {
					return nil, fmt.Errorf("messages: messages[%d].content[%d] tool_use input is invalid", index, j)
				}
				args = string(encoded)
			}
			calls = append(calls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			})
		case "tool_result":
			id, _ := b["tool_use_id"].(string)
			if id == "" {
				return nil, fmt.Errorf("messages: messages[%d].content[%d] tool_result needs a tool_use_id", index, j)
			}
			flush()
			turns = append(turns, map[string]any{
				"role": "tool", "tool_call_id": id, "content": messagesToolResult(b["content"]),
			})
		case "thinking", "redacted_thinking":
			// Signatures are bound to the original request and cannot round-trip
			// through a chat upstream.
		default:
			return nil, fmt.Errorf("messages: messages[%d].content[%d].type %q is unsupported", index, j, b["type"])
		}
	}
	flush()
	return turns, nil
}

// messagesParts degrades a single text part to a plain string, which is the
// shape every chat upstream accepts.
func messagesParts(parts []any) any {
	if len(parts) == 1 {
		if part, ok := parts[0].(map[string]any); ok && part["type"] == "text" {
			return part["text"]
		}
	}
	return parts
}

func messagesImage(index, j int, raw any) (map[string]any, error) {
	source, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("messages: messages[%d].content[%d].image source is invalid", index, j)
	}
	switch source["type"] {
	case "base64":
		media, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if media == "" || data == "" {
			return nil, fmt.Errorf("messages: messages[%d].content[%d].image needs media_type and data", index, j)
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + media + ";base64," + data}}, nil
	case "url":
		url, _ := source["url"].(string)
		if url == "" {
			return nil, fmt.Errorf("messages: messages[%d].content[%d].image needs a url", index, j)
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}, nil
	}
	return nil, fmt.Errorf("messages: messages[%d].content[%d].image source type is unsupported", index, j)
}

// messagesToolResult keeps a tool result's payload. A plain string stays a
// string; a block list keeps its images, which the model needs on the next
// turn.
func messagesToolResult(raw any) any {
	if text, ok := raw.(string); ok {
		return text
	}
	blocks, ok := raw.([]any)
	if !ok {
		return ""
	}
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		m, ok := block.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "text":
			if text, ok := m["text"].(string); ok && text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
		case "image":
			if part, err := messagesImage(0, 0, m["source"]); err == nil {
				parts = append(parts, part)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return messagesParts(parts)
}

// messagesTools converts Anthropic tools to the chat function shape. A tool
// without an input_schema is a server tool (web_search_*, bash_*) with no chat
// equivalent, so it is rejected rather than dropped: silently removing a tool
// the model was told it has changes the task.
func messagesTools(raw any) ([]any, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("messages: tools must be an array")
	}
	tools := make([]any, 0, len(list))
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages: tool is not an object")
		}
		schema, hasSchema := tool["input_schema"].(map[string]any)
		if !hasSchema {
			return nil, fmt.Errorf("messages: tool %v is a server tool, which this gateway does not support", tool["name"])
		}
		name, _ := tool["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("messages: tool needs a name")
		}
		fn := map[string]any{"name": name, "parameters": schema}
		if description, ok := tool["description"].(string); ok {
			fn["description"] = description
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	return tools, nil
}

func messagesToolChoice(raw any) (any, error) {
	if raw == nil {
		return nil, nil
	}
	choice, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("messages: tool_choice is invalid")
	}
	switch choice["type"] {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "none":
		return "none", nil
	case "tool":
		name, _ := choice["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("messages: tool_choice needs a tool name")
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}, nil
	}
	return nil, fmt.Errorf("messages: tool_choice type %v is unsupported", choice["type"])
}
