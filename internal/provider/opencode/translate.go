package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func toAnthropicMessagesBody(req provider.ChatRequest) ([]byte, error) {
	root := requestRoot(req)
	messages, err := requiredSlice(root, "messages")
	if err != nil {
		return nil, err
	}

	out := map[string]any{
		"model":      req.Model,
		"max_tokens": 4096,
		"stream":     req.Stream,
	}
	if value, ok := provider.FirstPresent(root, "max_tokens", "max_completion_tokens"); ok {
		out["max_tokens"] = value
	}
	for _, key := range []string{"temperature", "top_p", "top_k", "stop_sequences"} {
		if value, ok := root[key]; ok && value != nil {
			out[key] = value
		}
	}
	if raw, ok := root["stop"]; ok && root["stop_sequences"] == nil {
		out["stop_sequences"] = raw
	}
	if raw, ok := root["tools"]; ok {
		tools, err := translateTools(raw)
		if err != nil {
			return nil, err
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if raw, ok := root["tool_choice"]; ok {
		choice, err := translateToolChoice(raw)
		if err != nil {
			return nil, err
		}
		out["tool_choice"] = choice
	}

	var system []any
	anthropicMessages := make([]any, 0, len(messages))
	for i, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			return nil, invalid("messages[%d] must be an object", i)
		}
		role, err := requiredString(message, "role", fmt.Sprintf("messages[%d]", i))
		if err != nil {
			return nil, err
		}
		switch role {
		case "system", "developer":
			blocks, err := textBlocks(message["content"], fmt.Sprintf("messages[%d].content", i))
			if err != nil {
				return nil, err
			}
			system = append(system, blocks...)
		case "user":
			blocks, err := contentBlocks(message["content"], "user", fmt.Sprintf("messages[%d].content", i))
			if err != nil {
				return nil, err
			}
			anthropicMessages = append(anthropicMessages, map[string]any{"role": "user", "content": blocks})
		case "assistant":
			blocks, err := assistantBlocks(message, i)
			if err != nil {
				return nil, err
			}
			anthropicMessages = append(anthropicMessages, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			block, err := toolResult(message, i)
			if err != nil {
				return nil, err
			}
			anthropicMessages = appendToolResult(anthropicMessages, block)
		default:
			return nil, invalid("messages[%d].role %q is unsupported", i, role)
		}
	}
	if len(system) > 0 {
		out["system"] = system
	}
	out["messages"] = anthropicMessages
	return json.Marshal(out)
}

func requestRoot(req provider.ChatRequest) map[string]any {
	switch {
	case req.Parsed != nil:
		root := make(map[string]any, len(req.Parsed))
		for key, value := range req.Parsed {
			if key == provider.OpenAIBodyCacheKey() {
				continue
			}
			root[key] = value
		}
		return root
	case len(req.Raw) > 0:
		var root map[string]any
		if err := json.Unmarshal(req.Raw, &root); err == nil && root != nil {
			return root
		}
	}
	messages := make([]any, 0, len(req.Messages))
	for _, message := range req.Messages {
		messages = append(messages, map[string]any{"role": message.Role, "content": message.Content})
	}
	return map[string]any{"messages": messages}
}

func textBlocks(raw any, path string) ([]any, error) {
	if text, ok := raw.(string); ok {
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
	parts, ok := raw.([]any)
	if !ok {
		return nil, invalid("%s must be a string or array", path)
	}
	out := make([]any, 0, len(parts))
	for i, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return nil, invalid("%s[%d] must be an object", path, i)
		}
		typeName, err := requiredString(part, "type", fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		if typeName != "text" {
			return nil, invalid("%s[%d].type %q is unsupported", path, i, typeName)
		}
		text, err := requiredString(part, "text", fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"type": "text", "text": text})
	}
	return out, nil
}

func contentBlocks(raw any, role, path string) ([]any, error) {
	if text, ok := raw.(string); ok {
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
	parts, ok := raw.([]any)
	if !ok {
		return nil, invalid("%s must be a string or array", path)
	}
	out := make([]any, 0, len(parts))
	for i, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return nil, invalid("%s[%d] must be an object", path, i)
		}
		block, err := contentBlock(part, role, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		out = append(out, block)
	}
	return out, nil
}

func contentBlock(part map[string]any, role, path string) (map[string]any, error) {
	typeName, err := requiredString(part, "type", path)
	if err != nil {
		return nil, err
	}
	switch typeName {
	case "text":
		text, err := requiredString(part, "text", path)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "text", "text": text}, nil
	case "image_url", "image":
		if role != "user" {
			return nil, invalid("%s image is unsupported for %s", path, role)
		}
		source, err := imageSource(part, path)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "image", "source": source}, nil
	default:
		return nil, invalid("%s.type %q is unsupported", path, typeName)
	}
}

func imageSource(part map[string]any, path string) (map[string]any, error) {
	if source, ok := part["source"].(map[string]any); ok {
		kind, _ := source["type"].(string)
		switch kind {
		case "base64":
			media, err := requiredString(source, "media_type", path+".source")
			if err != nil {
				return nil, err
			}
			data, err := requiredString(source, "data", path+".source")
			if err != nil {
				return nil, err
			}
			return map[string]any{"type": "base64", "media_type": media, "data": data}, nil
		case "url":
			url, err := requiredString(source, "url", path+".source")
			if err != nil {
				return nil, err
			}
			return map[string]any{"type": "url", "url": url}, nil
		default:
			return nil, invalid("%s.source.type %q is unsupported", path, kind)
		}
	}
	raw := part["image_url"]
	if text, ok := raw.(string); ok {
		raw = map[string]any{"url": text}
	}
	image, ok := raw.(map[string]any)
	if !ok {
		return nil, invalid("%s.image_url must be an object", path)
	}
	url, err := requiredString(image, "url", path+".image_url")
	if err != nil {
		return nil, err
	}
	if media, data, ok := strings.Cut(url, ";base64,"); ok && strings.HasPrefix(media, "data:") {
		return map[string]any{"type": "base64", "media_type": strings.TrimPrefix(media, "data:"), "data": data}, nil
	}
	return map[string]any{"type": "url", "url": url}, nil
}

func assistantBlocks(message map[string]any, index int) ([]any, error) {
	var blocks []any
	if content, ok := message["content"]; ok && content != nil {
		if text, isText := content.(string); isText {
			if text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
		} else {
			parts, ok := content.([]any)
			if !ok {
				return nil, invalid("messages[%d].content must be a string or array", index)
			}
			for i, rawPart := range parts {
				part, ok := rawPart.(map[string]any)
				if !ok {
					return nil, invalid("messages[%d].content[%d] must be an object", index, i)
				}
				typeName, err := requiredString(part, "type", fmt.Sprintf("messages[%d].content[%d]", index, i))
				if err != nil {
					return nil, err
				}
				switch typeName {
				case "text":
					text, err := requiredString(part, "text", fmt.Sprintf("messages[%d].content[%d]", index, i))
					if err != nil {
						return nil, err
					}
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				case "reasoning", "thinking":
					text, _ := part["text"].(string)
					if text == "" {
						text, _ = part["thinking"].(string)
					}
					if text == "" {
						text, _ = part["reasoning"].(string)
					}
					if text != "" {
						blocks = append(blocks, map[string]any{"type": "thinking", "thinking": text})
					}
				default:
					return nil, invalid("messages[%d].content[%d].type %q is unsupported", index, i, typeName)
				}
			}
		}
	}
	if raw, ok := message["reasoning_content"].(string); ok && raw != "" {
		blocks = append([]any{map[string]any{"type": "thinking", "thinking": raw}}, blocks...)
	}
	if raw, ok := message["tool_calls"]; ok {
		calls, ok := raw.([]any)
		if !ok {
			return nil, invalid("messages[%d].tool_calls must be an array", index)
		}
		for i, rawCall := range calls {
			path := fmt.Sprintf("messages[%d].tool_calls[%d]", index, i)
			call, ok := rawCall.(map[string]any)
			if !ok {
				return nil, invalid("%s must be an object", path)
			}
			typeName, err := requiredString(call, "type", path)
			if err != nil {
				return nil, err
			}
			if typeName != "function" {
				return nil, invalid("%s.type %q is unsupported", path, typeName)
			}
			id, err := requiredString(call, "id", path)
			if err != nil {
				return nil, err
			}
			function, ok := call["function"].(map[string]any)
			if !ok {
				return nil, invalid("%s.function must be an object", path)
			}
			name, err := requiredString(function, "name", path+".function")
			if err != nil {
				return nil, err
			}
			arguments, _ := function["arguments"].(string)
			input := any(map[string]any{})
			if strings.TrimSpace(arguments) != "" {
				if err := json.Unmarshal([]byte(arguments), &input); err != nil {
					return nil, invalid("%s.function.arguments must be JSON", path)
				}
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
		}
	}
	if len(blocks) == 0 {
		blocks = []any{map[string]any{"type": "text", "text": ""}}
	}
	return blocks, nil
}

func toolResult(message map[string]any, index int) (map[string]any, error) {
	path := fmt.Sprintf("messages[%d]", index)
	id, err := requiredString(message, "tool_call_id", path)
	if err != nil {
		return nil, err
	}
	content, err := toolResultContent(message["content"], path+".content")
	if err != nil {
		return nil, err
	}
	block := map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
	if isError, ok := message["is_error"].(bool); ok && isError {
		block["is_error"] = true
	}
	return block, nil
}

func toolResultContent(raw any, path string) (any, error) {
	switch content := raw.(type) {
	case nil:
		return "", nil
	case string:
		return content, nil
	case []any:
		blocks, err := contentBlocks(content, "user", path)
		if err != nil {
			return nil, err
		}
		return blocks, nil
	default:
		encoded, err := json.Marshal(content)
		if err != nil {
			return nil, invalid("%s is unsupported", path)
		}
		return string(encoded), nil
	}
}

func appendToolResult(messages []any, block map[string]any) []any {
	if n := len(messages); n > 0 {
		last := messages[n-1].(map[string]any)
		if last["role"] == "user" {
			if blocks, ok := last["content"].([]any); ok && len(blocks) > 0 {
				if first, ok := blocks[0].(map[string]any); ok && first["type"] == "tool_result" {
					last["content"] = append(blocks, block)
					return messages
				}
			}
		}
	}
	return append(messages, map[string]any{"role": "user", "content": []any{block}})
}

func translateTools(raw any) ([]any, error) {
	tools, ok := raw.([]any)
	if !ok {
		return nil, invalid("tools must be an array")
	}
	out := make([]any, 0, len(tools))
	for i, rawTool := range tools {
		path := fmt.Sprintf("tools[%d]", i)
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return nil, invalid("%s must be an object", path)
		}
		function := tool
		if nested, ok := tool["function"].(map[string]any); ok {
			function = nested
		} else if typeName, _ := tool["type"].(string); typeName != "" && typeName != "function" && typeName != "custom" {
			return nil, invalid("%s.type %q is unsupported", path, typeName)
		}
		name, err := requiredString(function, "name", path)
		if err != nil {
			return nil, err
		}
		translated := map[string]any{"name": name}
		if description, ok := function["description"].(string); ok {
			translated["description"] = description
		}
		schema := function["parameters"]
		if schema == nil {
			schema = function["input_schema"]
		}
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if _, ok := schema.(map[string]any); !ok {
			return nil, invalid("%s.parameters must be an object", path)
		}
		translated["input_schema"] = schema
		out = append(out, translated)
	}
	return out, nil
}

func translateToolChoice(raw any) (any, error) {
	if choice, ok := raw.(string); ok {
		switch choice {
		case "auto", "none", "any", "required":
			kind := choice
			if choice == "required" {
				kind = "any"
			}
			return map[string]any{"type": kind}, nil
		default:
			return nil, invalid("tool_choice %q is unsupported", choice)
		}
	}
	choice, ok := raw.(map[string]any)
	if !ok {
		return nil, invalid("tool_choice must be a string or object")
	}
	typeName, _ := choice["type"].(string)
	switch typeName {
	case "", "auto", "none", "any":
		if typeName == "" {
			typeName = "auto"
		}
		return map[string]any{"type": typeName}, nil
	case "function", "tool":
		name, _ := choice["name"].(string)
		if name == "" {
			if function, ok := choice["function"].(map[string]any); ok {
				name, _ = function["name"].(string)
			}
		}
		if name == "" {
			return nil, invalid("tool_choice.name is required")
		}
		return map[string]any{"type": "tool", "name": name}, nil
	default:
		return nil, invalid("tool_choice.type %q is unsupported", typeName)
	}
}

func streamAnthropicToChat(ctx context.Context, r io.Reader, model, account string, w http.ResponseWriter) error {
	state := newAnthropicState(model)
	flusher, _ := w.(http.Flusher)
	err := provider.ReadSSE(ctx, r, "opencode", func(eventType string, data []byte) error {
		chunks, err := state.consume(eventType, data)
		if err != nil {
			return err
		}
		for _, chunk := range chunks {
			if err := provider.WriteSSEChunk(w, chunk); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if state.failed {
		return errors.New(state.failure)
	}
	if !state.completed {
		return errors.New("opencode: incomplete anthropic stream")
	}
	provider.ReportUsage(ctx, state.usage.tokenUsage(account))
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func completeAnthropicToChat(ctx context.Context, r io.Reader, model, account string, w http.ResponseWriter) error {
	body, err := provider.ReadCompletionBody(r)
	if err != nil {
		return err
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("opencode: malformed anthropic response")
	}
	if errType, _ := resp["type"].(string); errType == "error" {
		return errors.New("opencode: upstream anthropic error")
	}
	id, _ := resp["id"].(string)
	if id == "" {
		id = "chatcmpl-" + provider.RandomID()
	}
	blocks, _ := resp["content"].([]any)
	var text strings.Builder
	var reasoning strings.Builder
	var calls []any
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("opencode: malformed anthropic content")
		}
		switch block["type"] {
		case "text":
			value, _ := block["text"].(string)
			text.WriteString(value)
		case "thinking":
			value, _ := block["thinking"].(string)
			reasoning.WriteString(value)
		case "tool_use":
			callID, _ := block["id"].(string)
			name, _ := block["name"].(string)
			args, err := json.Marshal(block["input"])
			if err != nil {
				return err
			}
			calls = append(calls, map[string]any{
				"index": len(calls), "id": callID, "type": "function",
				"function": map[string]any{"name": name, "arguments": string(args)},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	usage := readAnthropicUsage(resp["usage"])
	provider.ReportUsage(ctx, usage.tokenUsage(account))
	response := map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": anthropicFinish(resp["stop_reason"], len(calls) > 0)}},
		"usage":   usage.usageMap(),
	}
	return json.NewEncoder(w).Encode(response)
}

type anthropicState struct {
	id        string
	model     string
	created   int64
	roleSent  bool
	index     int
	toolIndex int
	block     string
	toolID    string
	toolName  string
	completed bool
	failed    bool
	failure   string
	usage     countedUsage
}

func newAnthropicState(model string) *anthropicState {
	return &anthropicState{id: "chatcmpl-" + provider.RandomID(), model: model, created: time.Now().Unix(), toolIndex: -1}
}

func (s *anthropicState) consume(eventType string, payload []byte) ([]map[string]any, error) {
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("opencode: malformed anthropic event")
	}
	if eventType == "" {
		eventType, _ = event["type"].(string)
	}
	if eventType == "" {
		return nil, fmt.Errorf("opencode: malformed anthropic event")
	}
	chunks := make([]map[string]any, 0, 2)
	switch eventType {
	case "message_start":
		if message, ok := event["message"].(map[string]any); ok {
			if id, ok := message["id"].(string); ok && id != "" {
				s.id = id
			}
			if model, ok := message["model"].(string); ok && model != "" {
				s.model = model
			}
			if _, ok := message["usage"]; ok {
				s.usage.merge(readAnthropicUsage(message["usage"]))
			}
		}
	case "content_block_start":
		block, _ := event["content_block"].(map[string]any)
		s.block, _ = block["type"].(string)
		if index, ok := provider.NumberAsInt(event["index"]); ok {
			s.index = index
		}
		if s.block == "tool_use" {
			s.toolIndex++
			s.toolID, _ = block["id"].(string)
			s.toolName, _ = block["name"].(string)
			chunks = append(chunks, s.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": s.toolIndex, "id": s.toolID, "type": "function",
				"function": map[string]any{"name": s.toolName, "arguments": ""},
			}}}, nil, nil))
		}
	case "content_block_delta":
		delta, _ := event["delta"].(map[string]any)
		kind, _ := delta["type"].(string)
		switch kind {
		case "text_delta":
			text, _ := delta["text"].(string)
			if text != "" {
				chunks = append(chunks, s.chunk(map[string]any{"content": text}, nil, nil))
			}
		case "thinking_delta":
			text, _ := delta["thinking"].(string)
			if text != "" {
				chunks = append(chunks, s.chunk(map[string]any{"reasoning_content": text}, nil, nil))
			}
		case "input_json_delta":
			partial, _ := delta["partial_json"].(string)
			if partial != "" {
				chunks = append(chunks, s.chunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": s.toolIndex, "function": map[string]any{"arguments": partial},
				}}}, nil, nil))
			}
		case "signature_delta":
		default:
			if kind != "" {
				return nil, invalid("anthropic delta %q is unsupported", kind)
			}
		}
	case "content_block_stop":
		s.block = ""
	case "message_delta":
		if _, ok := event["usage"]; ok {
			s.usage.merge(readAnthropicUsage(event["usage"]))
		}
		delta, _ := event["delta"].(map[string]any)
		if reason, ok := delta["stop_reason"].(string); ok && reason != "" {
			s.completed = true
			chunks = append(chunks, s.chunk(map[string]any{}, anthropicFinish(reason, s.toolIndex >= 0), s.usage.usageMap()))
		}
	case "message_stop":
		if !s.completed {
			s.completed = true
			chunks = append(chunks, s.chunk(map[string]any{}, anthropicFinish("", s.toolIndex >= 0), s.usage.usageMap()))
		}
	case "ping":
	case "error":
		s.failed = true
		s.failure = anthropicError(event)
	default:
		return nil, invalid("anthropic event %q is unsupported", eventType)
	}
	if len(chunks) > 0 && !s.roleSent {
		s.roleSent = true
		chunks = append([]map[string]any{s.chunk(map[string]any{"role": "assistant"}, nil, nil)}, chunks...)
	}
	return chunks, nil
}

func (s *anthropicState) chunk(delta map[string]any, finish any, usage map[string]any) map[string]any {
	out := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out
}

func anthropicFinish(reason any, tools bool) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "end_turn", "stop_sequence", "refusal":
		return "stop"
	}
	if tools {
		return "tool_calls"
	}
	return "stop"
}

func anthropicError(event map[string]any) string {
	if nested, ok := event["error"].(map[string]any); ok {
		if message, ok := nested["message"].(string); ok && message != "" {
			return "opencode: " + message
		}
	}
	return "opencode: upstream anthropic error"
}

type countedUsage struct {
	input        int
	output       int
	cached       int
	cacheCreate  int
	reasoning    int
	inputSet     bool
	outputSet    bool
	cachedSet    bool
	createSet    bool
	reasoningSet bool
	present      bool
}

func (u *countedUsage) merge(next countedUsage) {
	if !next.present {
		return
	}
	u.present = true
	if next.inputSet {
		u.input, u.inputSet = next.input, true
	}
	if next.outputSet {
		u.output, u.outputSet = next.output, true
	}
	if next.cachedSet {
		u.cached, u.cachedSet = next.cached, true
	}
	if next.createSet {
		u.cacheCreate, u.createSet = next.cacheCreate, true
	}
	if next.reasoningSet {
		u.reasoning, u.reasoningSet = next.reasoning, true
	}
}

func (u countedUsage) tokenUsage(account string) provider.TokenUsage {
	return provider.TokenUsage{
		InputTokens:              u.input,
		OutputTokens:             u.output,
		CachedTokens:             u.cached,
		CacheCreationInputTokens: u.cacheCreate,
		ReasoningTokens:          u.reasoning,
		Account:                  account,
		Present:                  u.present,
	}
}

func (u countedUsage) usageMap() map[string]any {
	out := map[string]any{
		"prompt_tokens": u.input, "completion_tokens": u.output, "total_tokens": u.input + u.output,
	}
	if u.cached > 0 || u.cacheCreate > 0 {
		details := map[string]any{}
		if u.cached > 0 {
			details["cached_tokens"] = u.cached
		}
		if u.cacheCreate > 0 {
			details["cache_creation_tokens"] = u.cacheCreate
		}
		out["prompt_tokens_details"] = details
	}
	if u.reasoning > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.reasoning}
	}
	return out
}

func readAnthropicUsage(raw any) countedUsage {
	data, ok := raw.(map[string]any)
	if !ok {
		return countedUsage{}
	}
	var usage countedUsage
	usage.present = true
	if value, ok := provider.NumberAsInt(provider.First(data["input_tokens"], data["prompt_tokens"])); ok {
		usage.input, usage.inputSet = value, true
	}
	if value, ok := provider.NumberAsInt(provider.First(data["output_tokens"], data["completion_tokens"])); ok {
		usage.output, usage.outputSet = value, true
	}
	if value, ok := provider.NumberAsInt(provider.First(data["cache_read_input_tokens"], data["cached_tokens"])); ok {
		usage.cached, usage.cachedSet = value, true
	}
	if value, ok := provider.NumberAsInt(data["cache_creation_input_tokens"]); ok {
		usage.cacheCreate, usage.createSet = value, true
	}
	if value, ok := provider.NumberAsInt(data["reasoning_tokens"]); ok {
		usage.reasoning, usage.reasoningSet = value, true
	}
	if details, ok := data["input_tokens_details"].(map[string]any); ok {
		if value, ok := provider.NumberAsInt(details["cached_tokens"]); ok {
			usage.cached, usage.cachedSet = value, true
		}
	}
	if details, ok := data["output_tokens_details"].(map[string]any); ok {
		if value, ok := provider.NumberAsInt(details["reasoning_tokens"]); ok {
			usage.reasoning, usage.reasoningSet = value, true
		}
	}
	return usage
}

func streamResponsesToChat(ctx context.Context, r io.Reader, model string, creds provider.Credentials, w http.ResponseWriter) error {
	state := newResponsesState(model)
	flusher, _ := w.(http.Flusher)
	err := provider.ReadSSE(ctx, r, "opencode", func(eventType string, data []byte) error {
		chunks, err := state.consume(eventType, data)
		if err != nil {
			return err
		}
		for _, chunk := range chunks {
			if err := provider.WriteSSEChunk(w, chunk); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return state.finish(ctx, creds.Identity(), w, flusher, true)
}

func completeResponsesToChat(ctx context.Context, r io.Reader, model string, creds provider.Credentials, w http.ResponseWriter) error {
	state := newResponsesState(model)
	if err := provider.ReadSSE(ctx, r, "opencode", func(eventType string, data []byte) error {
		_, err := state.consume(eventType, data)
		return err
	}); err != nil {
		return err
	}
	return state.finish(ctx, creds.Identity(), w, nil, false)
}

type toolCall struct {
	index     int
	id        string
	itemID    string
	name      string
	arguments strings.Builder
	announced bool
}

type responsesState struct {
	id           string
	model        string
	created      int64
	roleSent     bool
	text         strings.Builder
	reasoning    strings.Builder
	calls        []*toolCall
	byItem       map[string]*toolCall
	byOutput     map[int]*toolCall
	finishReason string
	events       int
	completed    bool
	failed       bool
	failure      string
	usage        countedUsage
	sawText      bool
	sawReason    bool
}

func newResponsesState(model string) *responsesState {
	return &responsesState{
		id: "chatcmpl-" + provider.RandomID(), model: model, created: time.Now().Unix(),
		byItem: map[string]*toolCall{}, byOutput: map[int]*toolCall{},
	}
}

func (s *responsesState) consume(eventType string, payload []byte) ([]map[string]any, error) {
	s.events++
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("opencode: malformed SSE event")
	}
	if eventType == "" {
		eventType, _ = event["type"].(string)
	}
	if eventType == "" {
		return nil, fmt.Errorf("opencode: malformed SSE event")
	}
	chunks := make([]map[string]any, 0, 2)
	switch eventType {
	case "response.created", "response.in_progress":
		s.noteResponse(event["response"])
	case "response.output_text.delta":
		delta, _ := event["delta"].(string)
		if delta != "" {
			s.text.WriteString(delta)
			s.sawText = true
			chunks = append(chunks, s.chunk(map[string]any{"content": delta}, nil, nil))
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_content_text.delta", "response.reasoning_text.delta":
		delta, _ := event["delta"].(string)
		if delta != "" {
			s.reasoning.WriteString(delta)
			s.sawReason = true
			chunks = append(chunks, s.chunk(map[string]any{"reasoning_content": delta}, nil, nil))
		}
	case "response.output_item.added":
		if item, ok := event["item"].(map[string]any); ok && item["type"] == "function_call" {
			call := s.addTool(event, item)
			chunks = append(chunks, s.announce(call, ""))
		}
	case "response.function_call_arguments.delta":
		delta, _ := event["delta"].(string)
		if call := s.findTool(event); call != nil && delta != "" {
			call.arguments.WriteString(delta)
			chunks = append(chunks, s.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": call.index, "function": map[string]any{"arguments": delta},
			}}}, nil, nil))
		}
	case "response.output_item.done":
		item, _ := event["item"].(map[string]any)
		switch item["type"] {
		case "reasoning":
			if !s.sawReason {
				if text := reasoningText(item); text != "" {
					s.reasoning.WriteString(text)
					s.sawReason = true
					chunks = append(chunks, s.chunk(map[string]any{"reasoning_content": text}, nil, nil))
				}
			}
		case "function_call":
			call := s.findTool(event)
			arguments, _ := item["arguments"].(string)
			if call == nil {
				call = s.addTool(event, item)
				call.arguments.WriteString(arguments)
				chunks = append(chunks, s.announce(call, arguments))
			} else if call.arguments.Len() == 0 && arguments != "" {
				call.arguments.WriteString(arguments)
				chunks = append(chunks, s.chunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": call.index, "function": map[string]any{"arguments": arguments},
				}}}, nil, nil))
			}
		case "message":
			if !s.sawText {
				if text := outputText(item); text != "" {
					s.text.WriteString(text)
					s.sawText = true
					chunks = append(chunks, s.chunk(map[string]any{"content": text}, nil, nil))
				}
			}
		}
	case "response.completed", "response.incomplete", "response.done":
		s.completed = true
		response, _ := event["response"].(map[string]any)
		s.noteResponse(response)
		s.applyOutput(response["output"])
		if _, ok := response["usage"]; ok {
			s.usage.merge(readResponsesUsage(response["usage"]))
		}
		s.finishReason = responsesFinish(eventType, response, len(s.calls) > 0)
		chunks = append(chunks, s.chunk(map[string]any{}, s.finishReason, s.usage.usageMap()))
	case "response.failed", "error":
		s.failed = true
		s.failure = responsesError(event)
	case "response.output_text.done", "response.function_call_arguments.done", "response.reasoning_summary_text.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.content_part.added", "response.content_part.done":
	default:
		return nil, invalid("responses event %q is unsupported", eventType)
	}
	if len(chunks) > 0 && !s.roleSent {
		s.roleSent = true
		chunks = append([]map[string]any{s.chunk(map[string]any{"role": "assistant"}, nil, nil)}, chunks...)
	}
	return chunks, nil
}

func (s *responsesState) noteResponse(raw any) {
	response, _ := raw.(map[string]any)
	if id, ok := response["id"].(string); ok && id != "" {
		s.id = id
	}
	if model, ok := response["model"].(string); ok && model != "" {
		s.model = model
	}
}

func (s *responsesState) applyOutput(raw any) {
	items, _ := raw.([]any)
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "message":
			if !s.sawText {
				if text := outputText(item); text != "" {
					s.text.WriteString(text)
					s.sawText = true
				}
			}
		case "reasoning":
			if !s.sawReason {
				if text := reasoningText(item); text != "" {
					s.reasoning.WriteString(text)
					s.sawReason = true
				}
			}
		case "function_call":
			if s.findTool(map[string]any{"item": item}) != nil {
				continue
			}
			call := s.addTool(nil, item)
			arguments, _ := item["arguments"].(string)
			call.arguments.WriteString(arguments)
		}
	}
}

func (s *responsesState) addTool(event map[string]any, item map[string]any) *toolCall {
	call := &toolCall{index: len(s.calls)}
	call.id, _ = item["call_id"].(string)
	if call.id == "" {
		call.id, _ = item["id"].(string)
	}
	if call.id == "" {
		call.id = "call_" + provider.RandomID()
	}
	call.itemID, _ = item["id"].(string)
	call.name, _ = item["name"].(string)
	s.calls = append(s.calls, call)
	if call.itemID != "" {
		s.byItem[call.itemID] = call
	}
	if index, ok := provider.NumberAsInt(event["output_index"]); ok {
		s.byOutput[index] = call
	}
	return call
}

func (s *responsesState) findTool(event map[string]any) *toolCall {
	if id, ok := event["item_id"].(string); ok {
		if call := s.byItem[id]; call != nil {
			return call
		}
	}
	if item, ok := event["item"].(map[string]any); ok {
		if id, ok := item["id"].(string); ok {
			if call := s.byItem[id]; call != nil {
				return call
			}
		}
		if id, ok := item["call_id"].(string); ok {
			for _, call := range s.calls {
				if call.id == id {
					return call
				}
			}
		}
	}
	if index, ok := provider.NumberAsInt(event["output_index"]); ok {
		if call := s.byOutput[index]; call != nil {
			return call
		}
	}
	if len(s.calls) == 1 {
		return s.calls[0]
	}
	return nil
}

func (s *responsesState) announce(call *toolCall, arguments string) map[string]any {
	call.announced = true
	return s.chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": call.index, "id": call.id, "type": "function",
		"function": map[string]any{"name": call.name, "arguments": arguments},
	}}}, nil, nil)
}

func (s *responsesState) chunk(delta map[string]any, finish any, usage map[string]any) map[string]any {
	out := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out
}

func (s *responsesState) finish(ctx context.Context, account string, w http.ResponseWriter, flusher http.Flusher, stream bool) error {
	if s.failed {
		return errors.New(s.failure)
	}
	if !s.completed {
		if s.events == 0 {
			return provider.NewHTTPStatusError(http.StatusBadGateway, "opencode: upstream stream closed before first payload")
		}
		return fmt.Errorf("opencode: incomplete SSE response")
	}
	provider.ReportUsage(ctx, s.usage.tokenUsage(account))
	if stream {
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	message := map[string]any{"role": "assistant", "content": s.text.String()}
	if s.reasoning.Len() > 0 {
		message["reasoning_content"] = s.reasoning.String()
	}
	if len(s.calls) > 0 {
		calls := make([]any, 0, len(s.calls))
		for _, call := range s.calls {
			calls = append(calls, map[string]any{
				"index": call.index, "id": call.id, "type": "function",
				"function": map[string]any{"name": call.name, "arguments": call.arguments.String()},
			})
		}
		message["tool_calls"] = calls
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"id": s.id, "object": "chat.completion", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": s.finishReason}},
		"usage":   s.usage.usageMap(),
	})
}

func outputText(item map[string]any) string {
	parts, _ := item["content"].([]any)
	var text strings.Builder
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if part["type"] == "output_text" || part["type"] == "text" {
			value, _ := part["text"].(string)
			text.WriteString(value)
		}
	}
	return text.String()
}

func reasoningText(item map[string]any) string {
	for _, key := range []string{"summary", "content"} {
		parts, _ := item[key].([]any)
		var text strings.Builder
		for _, raw := range parts {
			part, _ := raw.(map[string]any)
			value, _ := part["text"].(string)
			text.WriteString(value)
		}
		if text.Len() > 0 {
			return text.String()
		}
	}
	return ""
}

func responsesFinish(eventType string, response map[string]any, tools bool) string {
	status, _ := response["status"].(string)
	if eventType == "response.incomplete" || status == "incomplete" {
		return "length"
	}
	if tools {
		return "tool_calls"
	}
	return "stop"
}

func responsesError(event map[string]any) string {
	if nested, ok := event["error"].(map[string]any); ok {
		if message, ok := nested["message"].(string); ok && message != "" {
			return "opencode: " + message
		}
	}
	if response, ok := event["response"].(map[string]any); ok {
		if nested, ok := response["error"].(map[string]any); ok {
			if message, ok := nested["message"].(string); ok && message != "" {
				return "opencode: " + message
			}
		}
	}
	return "opencode: upstream response failed"
}

func readResponsesUsage(raw any) countedUsage {
	data, ok := raw.(map[string]any)
	if !ok {
		return countedUsage{}
	}
	var usage countedUsage
	usage.present = true
	if value, ok := provider.NumberAsInt(provider.First(data["input_tokens"], data["prompt_tokens"])); ok {
		usage.input, usage.inputSet = value, true
	}
	if value, ok := provider.NumberAsInt(provider.First(data["output_tokens"], data["completion_tokens"])); ok {
		usage.output, usage.outputSet = value, true
	}
	if value, ok := provider.NumberAsInt(data["cache_creation_input_tokens"]); ok {
		usage.cacheCreate, usage.createSet = value, true
	}
	if details, ok := data["input_tokens_details"].(map[string]any); ok {
		if value, ok := provider.NumberAsInt(details["cached_tokens"]); ok {
			usage.cached, usage.cachedSet = value, true
		}
	}
	if details, ok := data["output_tokens_details"].(map[string]any); ok {
		if value, ok := provider.NumberAsInt(details["reasoning_tokens"]); ok {
			usage.reasoning, usage.reasoningSet = value, true
		}
	}
	return usage
}

func requiredSlice(object map[string]any, key string) ([]any, error) {
	raw, ok := object[key]
	if !ok {
		return nil, invalid("%s is required", key)
	}
	value, ok := raw.([]any)
	if !ok {
		return nil, invalid("%s must be an array", key)
	}
	return value, nil
}

func requiredString(object map[string]any, key, path string) (string, error) {
	raw, ok := object[key]
	if !ok {
		return "", invalid("%s.%s is required", path, key)
	}
	value, ok := raw.(string)
	if !ok || value == "" {
		return "", invalid("%s.%s must be a string", path, key)
	}
	return value, nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("opencode: "+format, args...)
}
