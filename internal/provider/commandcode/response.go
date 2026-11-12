package commandcode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type ccEvent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Reasoning string          `json:"reasoning"`
	Delta     json.RawMessage `json:"delta"`
	Item      json.RawMessage `json:"item"`
	ToolCall  *ccToolCall     `json:"toolCall"`
	Usage     json.RawMessage `json:"usage"`
	Error     *ccError        `json:"error"`
	Message   string          `json:"message"`
}

type ccToolCall struct {
	ID         string          `json:"id"`
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name"`
	ToolName   string          `json:"toolName"`
	Input      json.RawMessage `json:"input"`
	Arguments  json.RawMessage `json:"arguments"`
}

type ccError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Status  int    `json:"status"`
}

func eventStatus(err *ccError) int {
	if err == nil {
		return http.StatusBadGateway
	}
	msg := strings.ToLower(err.Message + " " + err.Code)
	if strings.Contains(msg, "unauthorized") || strings.Contains(msg, "invalid api") || strings.Contains(msg, "401") {
		return http.StatusUnauthorized
	}
	if strings.Contains(msg, "payment") || strings.Contains(msg, "billing") || strings.Contains(msg, "402") {
		return http.StatusPaymentRequired
	}
	if strings.Contains(msg, "quota") || strings.Contains(msg, "permission") || strings.Contains(msg, "forbidden") || strings.Contains(msg, "403") {
		return http.StatusForbidden
	}
	if strings.Contains(msg, "not found") || strings.Contains(msg, "404") {
		return http.StatusNotFound
	}
	if strings.Contains(msg, "rate") || strings.Contains(msg, "429") {
		return http.StatusTooManyRequests
	}
	if err.Status >= 400 {
		return err.Status
	}
	return http.StatusBadGateway
}

func translateNDJSON(ctx context.Context, reader io.Reader, model, accountID string, stream bool, w io.Writer, seen *bool) (provider.TokenUsage, error) {
	state := newGenerateState(model)
	usage, err := consumeNDJSON(ctx, reader, state, func(chunks []map[string]any) error {
		if !stream {
			return nil
		}
		for _, chunk := range chunks {
			payload, err := json.Marshal(chunk)
			if err != nil {
				return err
			}
			*seen = true
			w.Write([]byte("data: "))
			w.Write(payload)
			w.Write([]byte("\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		return nil
	})
	if err != nil {
		return provider.TokenUsage{}, err
	}
	usage.Account = accountID
	provider.ReportUsage(ctx, usage)
	if state.failed {
		return provider.TokenUsage{}, provider.NewHTTPStatusError(eventStatus(nil), "commandcode: "+state.failure)
	}
	if stream && !state.completed && !state.hasEvents() {
		return usage, provider.NewHTTPStatusError(http.StatusBadGateway, "commandcode: upstream stream closed before first payload")
	}
	if stream && !state.completed {
		return usage, provider.NewHTTPStatusError(http.StatusBadGateway, "commandcode: incomplete SSE response")
	}
	return usage, nil
}

// consumeNDJSON drives the NDJSON event loop over reader into state,
// invoking emit for each event's SSE chunks. It returns the accumulated
// usage.
func consumeNDJSON(ctx context.Context, reader io.Reader, state *generateState, emit func([]map[string]any) error) (provider.TokenUsage, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 50*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return provider.TokenUsage{}, err
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		var ev ccEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		chunks, _, err := state.consume(ev)
		if err != nil {
			return provider.TokenUsage{}, provider.NewHTTPStatusError(eventStatus(ev.Error), "commandcode: "+state.failure)
		}
		if emit != nil {
			if err := emit(chunks); err != nil {
				return provider.TokenUsage{}, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return provider.TokenUsage{}, err
	}
	if state.failed {
		return provider.TokenUsage{}, provider.NewHTTPStatusError(eventStatus(nil), "commandcode: "+state.failure)
	}
	return state.tokenUsage(""), nil
}

type generateState struct {
	model            string
	id               string
	text             strings.Builder
	reasoning        strings.Builder
	finishReason     string
	toolCalls        []*ccToolAccum
	toolByID         map[string]*ccToolAccum
	promptTokens     int
	completionTokens int
	totalTokens      int
	failed           bool
	failure          string
	completed        bool
	events           int
	usagePresent     bool
}

type ccToolAccum struct {
	index int
	id    string
	name  string
	args  strings.Builder
}

func newGenerateState(model string) *generateState {
	return &generateState{
		model:    model,
		id:       "chatcmpl-" + newThreadID(),
		toolByID: map[string]*ccToolAccum{},
	}
}

func (s *generateState) hasEvents() bool { return s.events > 0 }

func (s *generateState) consume(ev ccEvent) ([]map[string]any, provider.TokenUsage, error) {
	s.events++
	chunks := []map[string]any{}
	usage := provider.TokenUsage{}
	switch ev.Type {
	case "text-delta":
		if d := eventText(ev); d != "" {
			s.text.WriteString(d)
			chunks = append(chunks, s.chunk(map[string]any{"content": d}, nil, nil))
		}
	case "reasoning-delta", "reasoning":
		if d := eventText(ev); d != "" {
			s.reasoning.WriteString(d)
			chunks = append(chunks, s.chunk(map[string]any{"reasoning_content": d}, nil, nil))
		}
	case "tool-input-start", "tool-call-start", "tool-call":
		call := s.ensureTool(ev)
		chunks = append(chunks, s.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": call.index, "id": call.id, "type": "function",
			"function": map[string]any{"name": call.name, "arguments": ""},
		}}}, nil, nil))
	case "tool-input-delta", "tool-call-delta":
		call := s.ensureTool(ev)
		if d := eventText(ev); d != "" {
			call.args.WriteString(d)
			chunks = append(chunks, s.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": call.index, "id": call.id,
				"function": map[string]any{"arguments": d},
			}}}, nil, nil))
		}
	case "finish", "finish-step", "done", "complete":
		if ev.Item != nil {
			if u, ok := parseUsage(ev.Item); ok {
				usage = u
				s.applyUsage(u)
			}
		}
		if ev.Usage != nil {
			if u, ok := parseUsage(ev.Usage); ok {
				usage = u
				s.applyUsage(u)
			}
		}
		if r := finishReason(ev); r != "" {
			s.finishReason = r
		} else if len(s.toolCalls) > 0 {
			s.finishReason = "tool_calls"
		} else {
			s.finishReason = "stop"
		}
		usageMap := usageMapFromState(s)
		if usage.Present {
			usageMap = map[string]any{
				"prompt_tokens": s.promptTokens, "completion_tokens": s.completionTokens, "total_tokens": s.promptTokens + s.completionTokens,
			}
		}
		chunks = append(chunks, s.chunk(nil, s.finishReason, usageMap))
		s.completed = true
	case "error":
		s.failed = true
		msg := ev.Message
		if msg == "" && ev.Error != nil {
			msg = ev.Error.Message
		}
		if msg == "" {
			msg = "upstream error"
		}
		s.failure = msg
		return nil, provider.TokenUsage{}, provider.NewHTTPStatusError(eventStatus(ev.Error), "commandcode: "+msg)
	}
	return chunks, usage, nil
}

func (s *generateState) ensureTool(ev ccEvent) *ccToolAccum {
	id := ""
	name := ""
	args := ""
	if ev.ToolCall != nil {
		tc := ev.ToolCall
		id = tc.ID
		if id == "" {
			id = tc.ToolCallID
		}
		name = tc.Name
		if name == "" {
			name = tc.ToolName
		}
		if len(tc.Input) > 0 && !bytes.Equal(bytes.TrimSpace(tc.Input), []byte("null")) {
			args = string(tc.Input)
		} else if len(tc.Arguments) > 0 {
			args = string(tc.Arguments)
		}
	}
	if len(ev.Item) > 0 {
		var m map[string]any
		if json.Unmarshal(ev.Item, &m) == nil {
			if v, _ := m["id"].(string); v != "" && id == "" {
				id = v
			}
			if v, _ := m["toolCallId"].(string); v != "" && id == "" {
				id = v
			}
			if v, _ := m["name"].(string); v != "" && name == "" {
				name = v
			}
			if v, _ := m["toolName"].(string); v != "" && name == "" {
				name = v
			}
			if v := m["input"]; v != nil && args == "" {
				if b, err := json.Marshal(v); err == nil {
					args = string(b)
				}
			}
		}
	}
	if id == "" {
		id = "call-" + newThreadID()[:8]
	}
	if existing, ok := s.toolByID[id]; ok {
		if name != "" && existing.name == "" {
			existing.name = name
		}
		if args != "" {
			existing.args.WriteString(args)
		}
		return existing
	}
	call := &ccToolAccum{index: len(s.toolCalls), id: id, name: name}
	call.args.WriteString(args)
	s.toolCalls = append(s.toolCalls, call)
	s.toolByID[id] = call
	return call
}

func eventText(ev ccEvent) string {
	if ev.Text != "" {
		return ev.Text
	}
	if ev.Reasoning != "" {
		return ev.Reasoning
	}
	if len(ev.Delta) > 0 {
		var m map[string]any
		if json.Unmarshal(ev.Delta, &m) == nil {
			if t, _ := m["text"].(string); t != "" {
				return t
			}
		}
		var s string
		if json.Unmarshal(ev.Delta, &s) == nil {
			return s
		}
	}
	if len(ev.Item) > 0 {
		var m map[string]any
		if json.Unmarshal(ev.Item, &m) == nil {
			if t, _ := m["text"].(string); t != "" {
				return t
			}
		}
	}
	return ""
}

func finishReason(ev ccEvent) string {
	if len(ev.Item) > 0 {
		var m map[string]any
		if json.Unmarshal(ev.Item, &m) == nil {
			if r, _ := m["finish_reason"].(string); r != "" {
				return r
			}
			if r, _ := m["finishReason"].(string); r != "" {
				return r
			}
			if r, _ := m["stopReason"].(string); r != "" {
				return r
			}
		}
	}
	return ""
}

func parseUsage(raw json.RawMessage) (provider.TokenUsage, bool) {
	var m struct {
		InputTokens      int             `json:"input_tokens"`
		OutputTokens     int             `json:"output_tokens"`
		PromptTokens     int             `json:"prompt_tokens"`
		CompletionTokens int             `json:"completion_tokens"`
		Usage            json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return provider.TokenUsage{}, false
	}
	if len(m.Usage) > 0 {
		if u, ok := parseUsageFlat(m.Usage); ok {
			return u, true
		}
	}
	u := usageFromCounts(m.InputTokens, m.OutputTokens, m.PromptTokens, m.CompletionTokens)
	if u == (provider.TokenUsage{}) {
		return provider.TokenUsage{}, false
	}
	u.Present = true
	return u, true
}

func parseUsageFlat(raw json.RawMessage) (provider.TokenUsage, bool) {
	var m struct {
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return provider.TokenUsage{}, false
	}
	u := usageFromCounts(m.InputTokens, m.OutputTokens, m.PromptTokens, m.CompletionTokens)
	if u == (provider.TokenUsage{}) {
		return provider.TokenUsage{}, false
	}
	u.Present = true
	return u, true
}

func usageFromCounts(input, output, prompt, completion int) provider.TokenUsage {
	if input == 0 {
		input = prompt
	}
	if output == 0 {
		output = completion
	}
	return provider.TokenUsage{InputTokens: input, OutputTokens: output}
}

func (s *generateState) applyUsage(u provider.TokenUsage) {
	s.promptTokens = u.InputTokens
	s.completionTokens = u.OutputTokens
	s.totalTokens = u.InputTokens + u.OutputTokens
	s.usagePresent = true
}

func usageMapFromState(s *generateState) map[string]any {
	if !s.usagePresent && s.promptTokens == 0 && s.completionTokens == 0 && s.totalTokens == 0 {
		return nil
	}
	return map[string]any{
		"prompt_tokens": s.promptTokens, "completion_tokens": s.completionTokens, "total_tokens": s.totalTokens,
	}
}

func (s *generateState) tokenUsage(account string) provider.TokenUsage {
	return provider.TokenUsage{
		InputTokens: s.promptTokens, OutputTokens: s.completionTokens,
		Account: account, Present: s.usagePresent,
	}
}

func (s *generateState) chunk(delta map[string]any, reason any, usage map[string]any) map[string]any {
	chunk := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": deltaOrEmpty(delta), "finish_reason": reason}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	return chunk
}

func deltaOrEmpty(delta map[string]any) map[string]any {
	if delta == nil {
		return map[string]any{}
	}
	return delta
}
