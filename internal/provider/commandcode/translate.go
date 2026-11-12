package commandcode

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// countedUsage accumulates one upstream usage object.
type countedUsage struct {
	input       int
	output      int
	cached      int
	cacheCreate int
	reasoning   int
	present     bool
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
		"prompt_tokens":     u.input,
		"completion_tokens": u.output,
		"total_tokens":      u.input + u.output,
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

// readUsage reads the CommandCode (AI SDK v5) usage shape.
func readUsage(raw any) countedUsage {
	data, ok := raw.(map[string]any)
	if !ok {
		return countedUsage{}
	}
	usage := countedUsage{present: true}
	if value, ok := numberAsInt(first(data["inputTokens"], data["prompt_tokens"], data["promptTokens"])); ok {
		usage.input = value
	}
	if value, ok := numberAsInt(first(data["outputTokens"], data["completion_tokens"], data["completionTokens"])); ok {
		usage.output = value
	}
	if details, ok := data["inputTokenDetails"].(map[string]any); ok {
		if value, ok := numberAsInt(details["cacheReadTokens"]); ok {
			usage.cached = value
		}
		if value, ok := numberAsInt(details["cacheWriteTokens"]); ok {
			usage.cacheCreate = value
		}
	}
	if details, ok := data["outputTokenDetails"].(map[string]any); ok {
		if value, ok := numberAsInt(details["reasoningTokens"]); ok {
			usage.reasoning = value
		}
	}
	return usage
}

func first(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func numberAsInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		if typed < 0 {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		n, err := typed.Int64()
		if err != nil || n < 0 {
			return 0, false
		}
		return int(n), true
	case int:
		return typed, typed >= 0
	case int64:
		return int(typed), typed >= 0
	}
	return 0, false
}

type toolCall struct {
	index     int
	id        string
	name      string
	arguments strings.Builder
}

type state struct {
	id          string
	model       string
	created     int64
	chunkIndex  int
	toolIndex   int
	toolIndexBy map[string]int
	tools       []*toolCall
	finish      string
	usage       countedUsage
	finished    bool
	text        strings.Builder
	reasoning   strings.Builder
}

func newState(model string) *state {
	return &state{
		id:          "chatcmpl-" + randomID(),
		model:       model,
		created:     time.Now().Unix(),
		toolIndexBy: map[string]int{},
	}
}

func (s *state) chunk(delta map[string]any, finish any, usage map[string]any) map[string]any {
	out := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out
}

// consume translates one upstream event into zero or more OpenAI chunks.
func (s *state) consume(event map[string]any) ([]map[string]any, error) {
	eventType, _ := event["type"].(string)
	if eventType == "" {
		return nil, nil
	}
	switch eventType {
	case "error":
		return nil, commandCodeError(event)
	case "text-delta":
		text, _ := event["text"].(string)
		if text == "" {
			text, _ = event["delta"].(string)
		}
		if text == "" {
			return nil, nil
		}
		s.text.WriteString(text)
		delta := map[string]any{"content": text}
		s.withRole(delta)
		return []map[string]any{s.chunk(delta, nil, nil)}, nil
	case "reasoning-delta":
		text, _ := event["text"].(string)
		if text == "" {
			text, _ = event["delta"].(string)
		}
		if text == "" {
			return nil, nil
		}
		s.reasoning.WriteString(text)
		delta := map[string]any{"reasoning_content": text}
		s.withRole(delta)
		return []map[string]any{s.chunk(delta, nil, nil)}, nil
	case "tool-input-start":
		call := s.startTool(event)
		delta := map[string]any{"tool_calls": []any{map[string]any{
			"index": call.index,
			"id":    call.id,
			"type":  "function",
			"function": map[string]any{
				"name":      call.name,
				"arguments": "",
			},
		}}}
		s.withRole(delta)
		return []map[string]any{s.chunk(delta, nil, nil)}, nil
	case "tool-input-delta":
		id, _ := event["id"].(string)
		if id == "" {
			id, _ = event["toolCallId"].(string)
		}
		index, ok := s.toolIndexBy[id]
		if !ok {
			return nil, nil
		}
		fragment, _ := event["delta"].(string)
		if fragment == "" {
			fragment, _ = event["inputTextDelta"].(string)
		}
		if fragment == "" {
			return nil, nil
		}
		s.tools[index].arguments.WriteString(fragment)
		return []map[string]any{s.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index":    index,
			"function": map[string]any{"arguments": fragment},
		}}}, nil, nil)}, nil
	case "tool-call":
		id, _ := event["toolCallId"].(string)
		if id == "" {
			id, _ = event["id"].(string)
		}
		if _, seen := s.toolIndexBy[id]; seen {
			return nil, nil
		}
		call := s.startTool(event)
		arguments := ""
		switch input := event["input"].(type) {
		case string:
			arguments = input
		default:
			encoded, _ := json.Marshal(input)
			arguments = string(encoded)
		}
		call.arguments.WriteString(arguments)
		delta := map[string]any{"tool_calls": []any{map[string]any{
			"index": call.index,
			"id":    call.id,
			"type":  "function",
			"function": map[string]any{
				"name":      call.name,
				"arguments": arguments,
			},
		}}}
		s.withRole(delta)
		return []map[string]any{s.chunk(delta, nil, nil)}, nil
	case "finish-step":
		if reason, ok := event["finishReason"].(string); ok && reason != "" {
			s.finish = openAIFinish(reason, len(s.tools) > 0)
		}
		if usage := readUsage(first(event["usage"], event["totalUsage"])); usage.present {
			s.usage = usage
		}
		return nil, nil
	case "finish":
		if s.finish == "" {
			reason, _ := event["finishReason"].(string)
			s.finish = openAIFinish(reason, len(s.tools) > 0)
		}
		if usage := readUsage(first(event["totalUsage"], event["usage"])); usage.present {
			s.usage = usage
		}
		s.finished = true
		return []map[string]any{s.chunk(map[string]any{}, s.finish, s.usage.usageMap())}, nil
	default:
		return nil, nil
	}
}

func (s *state) withRole(delta map[string]any) {
	if s.chunkIndex == 0 {
		delta["role"] = "assistant"
	}
	s.chunkIndex++
}

func (s *state) startTool(event map[string]any) *toolCall {
	id, _ := event["id"].(string)
	if id == "" {
		id, _ = event["toolCallId"].(string)
	}
	if id == "" {
		id = fmt.Sprintf("call_%d", s.toolIndex)
	}
	name, _ := event["toolName"].(string)
	call := &toolCall{index: s.toolIndex, id: id, name: name}
	s.toolIndexBy[id] = call.index
	s.toolIndex++
	s.tools = append(s.tools, call)
	return call
}

func openAIFinish(reason string, tools bool) string {
	switch reason {
	case "tool-calls", "tool_calls":
		return "tool_calls"
	case "length", "max-tokens", "max_tokens":
		return "length"
	case "content-filter", "content_filter":
		return "content_filter"
	case "stop", "":
		if tools {
			return "tool_calls"
		}
		return "stop"
	}
	if tools {
		return "tool_calls"
	}
	return "stop"
}

// commandCodeError maps an upstream error event to a typed status error so the
// combo engine can fail over instead of emitting content.
func commandCodeError(event map[string]any) error {
	message := errorMessage(event)
	code := 0
	if nested, ok := event["error"].(map[string]any); ok {
		if value, ok := numberAsInt(first(nested["statusCode"], nested["status"])); ok {
			code = value
		}
	}
	if value, ok := numberAsInt(first(event["statusCode"], event["status"])); ok {
		code = value
	}
	if code < 400 || code > 599 {
		code = classifyMessage(message)
	}
	if message == "" {
		message = "commandcode: upstream error"
	}
	return provider.NewHTTPStatusError(code, message)
}

func errorMessage(event map[string]any) string {
	value := first(event["error"], event["message"])
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		if message, ok := typed["message"].(string); ok && message != "" {
			return message
		}
		if nested, ok := typed["error"].(string); ok {
			return nested
		}
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	}
	return ""
}

func classifyMessage(message string) int {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "too many requests"):
		return http.StatusTooManyRequests
	case strings.Contains(lower, "unauthorized"), strings.Contains(lower, "invalid api key"), strings.Contains(lower, "authentication"):
		return http.StatusUnauthorized
	case strings.Contains(lower, "payment required"), strings.Contains(lower, "billing"):
		return http.StatusPaymentRequired
	case strings.Contains(lower, "quota"), strings.Contains(lower, "forbidden"), strings.Contains(lower, "permission"):
		return http.StatusForbidden
	case strings.Contains(lower, "not found"):
		return http.StatusNotFound
	default:
		return http.StatusServiceUnavailable
	}
}

func writeCompletion(w io.Writer, s *state) error {
	message := map[string]any{"role": "assistant"}
	if s.text.Len() > 0 {
		message["content"] = s.text.String()
	} else {
		message["content"] = ""
	}
	if s.reasoning.Len() > 0 {
		message["reasoning_content"] = s.reasoning.String()
	}
	if len(s.tools) > 0 {
		calls := make([]any, 0, len(s.tools))
		for _, call := range s.tools {
			calls = append(calls, map[string]any{
				"id":   call.id,
				"type": "function",
				"function": map[string]any{
					"name":      call.name,
					"arguments": call.arguments.String(),
				},
			})
		}
		message["tool_calls"] = calls
	}
	finish := s.finish
	if finish == "" {
		finish = openAIFinish("", len(s.tools) > 0)
	}
	body := map[string]any{
		"id":      s.id,
		"object":  "chat.completion",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": s.usage.usageMap(),
	}
	return json.NewEncoder(w).Encode(body)
}

func randomID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value)
}
