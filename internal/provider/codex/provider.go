package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

const (
	DefaultResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
	DefaultModelsURL    = "https://raw.githubusercontent.com/openai/codex/main/codex-rs/models-manager/models.json"
	ClientVersion       = "0.154.0"
	Originator          = "codex_cli_rs"
	UserAgent           = Originator + "/" + ClientVersion + " (OmniGo)"
	BetaVersion         = "responses=experimental"
)

var DefaultModels = []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.2"}

type Provider struct {
	name         string
	responsesURL string
	modelsURL    string
	client       *http.Client
	stream       *http.Client
	idle         time.Duration
	store        provider.CredStore
	pool         provider.AccountPool
	tokens       sync.Map
}

type streamState struct {
	id             string
	model          string
	created        int64
	roleSent       bool
	text           strings.Builder
	reasoning      strings.Builder
	toolCalls      []*toolCall
	toolByItem     map[string]*toolCall
	toolByOutput   map[int]*toolCall
	finishReason   string
	usage          usage
	completed      bool
	failed         bool
	failureMessage string
}

type toolCall struct {
	Index     int
	ID        string
	ItemID    string
	Name      string
	Arguments strings.Builder
}

type usage struct {
	PromptTokens             int
	CompletionTokens         int
	TotalTokens              int
	CachedTokens             int
	ReasoningTokens          int
	CacheCreationInputTokens int
}

func New(cfg provider.Config, store provider.CredStore) provider.Provider {
	responsesURL := strings.TrimRight(cfg.BaseURL, "/")
	if responsesURL == "" {
		responsesURL = DefaultResponsesURL
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout, Transport: cfg.Transport}
	return &Provider{
		name:         cfg.Name,
		responsesURL: responsesURL,
		modelsURL:    DefaultModelsURL,
		client:       client,
		stream:       provider.StreamClient(client),
		idle:         timeout,
		store:        store,
	}
}

func (p *Provider) Name() string { return p.name }

func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	fallback := fallbackModels()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.modelsURL, nil)
	if err != nil {
		return fallback, nil
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fallback, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fallback, nil
	}
	var catalog struct {
		Models []struct {
			Slug           string `json:"slug"`
			DisplayName    string `json:"display_name"`
			Visibility     string `json:"visibility"`
			SupportedInAPI bool   `json:"supported_in_api"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&catalog); err != nil {
		return fallback, nil
	}
	models := make([]provider.Model, 0, len(catalog.Models))
	seen := make(map[string]bool, len(catalog.Models))
	for _, model := range catalog.Models {
		if model.Slug == "" || model.Visibility != "list" || !model.SupportedInAPI || seen[model.Slug] {
			continue
		}
		seen[model.Slug] = true
		name := model.DisplayName
		if name == "" {
			name = model.Slug
		}
		models = append(models, provider.Model{ID: model.Slug, Name: name})
	}
	if len(models) == 0 {
		return fallback, nil
	}
	return models, nil
}

func fallbackModels() []provider.Model {
	models := make([]provider.Model, 0, len(DefaultModels))
	for _, id := range DefaultModels {
		models = append(models, provider.Model{ID: id, Name: id})
	}
	return models
}

func (p *Provider) Test(ctx context.Context) provider.TestResult {
	start := time.Now()
	accounts := p.pool.Available(p.store)
	var lastErr error
	for _, account := range accounts {
		creds, err := p.tokenManager(account).EnsureFreshToken(ctx)
		if err == nil && creds.AccountID == "" {
			err = fmt.Errorf("codex: account ID is missing")
		}
		if err == nil {
			return provider.TestResult{OK: true, LatencyMS: time.Since(start).Milliseconds()}
		}
		lastErr = err
		p.pool.MarkFailed(account, err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("codex: not authenticated")
	}
	return provider.TestResult{LatencyMS: time.Since(start).Milliseconds(), Error: lastErr.Error()}
}

func (p *Provider) ChatCompletion(ctx context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request, err := ToResponsesRequest(req)
	if err != nil {
		return err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("codex: encode upstream request: %w", err)
	}
	accounts := p.pool.Available(p.store)
	if len(accounts) == 0 {
		return fmt.Errorf("codex: not authenticated")
	}
	var lastErr error
	for _, account := range accounts {
		attempt := provider.NewStreamingAttemptWriter(w, req.Stream)
		lastErr = p.chatWithAccount(ctx, req, body, account, attempt)
		if lastErr == nil {
			return attempt.Commit(w)
		}
		if attempt.Committed() {
			return lastErr
		}
		if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
			return lastErr
		}
		p.pool.MarkFailed(account, lastErr)
	}
	return lastErr
}

func (p *Provider) chatWithAccount(ctx context.Context, req provider.ChatRequest, body []byte, account provider.Credentials, w http.ResponseWriter) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	tokens := p.tokenManager(account)
	creds, err := tokens.EnsureFreshToken(ctx)
	if err != nil {
		return err
	}
	// Armed after token refresh so the idle window covers the generation
	// exchange only, not credential work.
	guard := provider.NewIdleGuard(p.idle, func() { cancel(provider.ErrUpstreamStall) })
	defer guard.Stop()
	sessionID := randomID()
	resp, err := p.send(ctx, creds, body, sessionID)
	if err != nil {
		return guard.Err(err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		creds, err = tokens.ForceRefreshToken(ctx, creds.AccessToken)
		if err != nil {
			return err
		}
		resp, err = p.send(ctx, creds, body, sessionID)
		if err != nil {
			return guard.Err(err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return newQuotaError(resp.Header)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("codex: upstream status %d", resp.StatusCode))
	}
	reader := guard.Wrap(resp.Body)
	if req.Stream {
		return p.streamResponse(ctx, reader, req.Model, w)
	}
	return p.completeResponse(ctx, reader, req.Model, w)
}

func (p *Provider) tokenManager(account provider.Credentials) *TokenManager {
	key := account.Identity()
	if key == "" {
		key = account.AccessToken
	}
	manager, _ := p.tokens.LoadOrStore(key, NewTokenManager(provider.ScopedStore(p.store, account)))
	return manager.(*TokenManager)
}

func (p *Provider) send(ctx context.Context, creds provider.Credentials, body []byte, sessionID string) (*http.Response, error) {
	if creds.AccessToken == "" {
		return nil, fmt.Errorf("codex: not authenticated")
	}
	if creds.AccountID == "" {
		return nil, fmt.Errorf("codex: account ID is missing")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.responsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("codex: create upstream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("chatgpt-account-id", creds.AccountID)
	req.Header.Set("Version", ClientVersion)
	req.Header.Set("originator", Originator)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("OpenAI-Beta", BetaVersion)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("session-id", sessionID)
	req.Header.Set("x-client-request-id", sessionID)
	resp, err := p.stream.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("codex: upstream request failed")
	}
	return resp, nil
}

func (p *Provider) streamResponse(ctx context.Context, body io.Reader, model string, w http.ResponseWriter) error {
	state := newStreamState(model)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	err := parseSSE(ctx, body, func(eventType string, data []byte) error {
		chunks, err := state.consume(eventType, data)
		if err != nil {
			return err
		}
		for _, chunk := range chunks {
			if err := writeSSEChunk(w, chunk); err != nil {
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
		return errors.New(state.failureMessage)
	}
	if !state.completed {
		return fmt.Errorf("codex: incomplete SSE response")
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func (p *Provider) completeResponse(ctx context.Context, body io.Reader, model string, w http.ResponseWriter) error {
	state := newStreamState(model)
	if err := parseSSE(ctx, body, func(eventType string, data []byte) error {
		_, err := state.consume(eventType, data)
		return err
	}); err != nil {
		return err
	}
	if state.failed {
		return errors.New(state.failureMessage)
	}
	if !state.completed {
		return fmt.Errorf("codex: incomplete SSE response")
	}
	message := map[string]any{"role": "assistant", "content": state.text.String()}
	if state.reasoning.Len() > 0 {
		message["reasoning_content"] = state.reasoning.String()
	}
	if len(state.toolCalls) > 0 {
		calls := make([]any, 0, len(state.toolCalls))
		for _, call := range state.toolCalls {
			calls = append(calls, map[string]any{
				"id": call.ID, "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": call.Arguments.String()},
			})
		}
		message["tool_calls"] = calls
	}
	response := map[string]any{
		"id": state.id, "object": "chat.completion", "created": state.created, "model": state.model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": state.finishReason}},
		"usage":   state.usageMap(),
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(response)
}

func newStreamState(model string) *streamState {
	return &streamState{
		id:           "chatcmpl-" + randomID(),
		model:        model,
		created:      time.Now().Unix(),
		toolByItem:   map[string]*toolCall{},
		toolByOutput: map[int]*toolCall{},
	}
}

func (s *streamState) consume(eventType string, payload []byte) ([]map[string]any, error) {
	parsed, err := parseResponseEvent(payload)
	if err != nil {
		return nil, fmt.Errorf("codex: malformed SSE event")
	}
	if eventType == "" {
		eventType = parsed.Type
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("codex: malformed SSE event")
	}
	chunks := make([]map[string]any, 0, 2)
	if !s.roleSent && emitsChunk(eventType) {
		s.roleSent = true
		chunks = append(chunks, s.chunk(map[string]any{"role": "assistant"}, nil, nil))
	}
	switch eventType {
	case "response.created", "response.in_progress":
		if response, ok := event["response"].(map[string]any); ok {
			if id, ok := response["id"].(string); ok && id != "" {
				s.id = id
			}
			if model, ok := response["model"].(string); ok && model != "" {
				s.model = model
			}
		}
	case "response.output_text.delta":
		delta, _ := event["delta"].(string)
		if delta != "" {
			s.text.WriteString(delta)
			chunks = append(chunks, s.chunk(map[string]any{"content": delta}, nil, nil))
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_content_text.delta", "response.reasoning_text.delta":
		delta, _ := event["delta"].(string)
		if delta != "" {
			s.reasoning.WriteString(delta)
			chunks = append(chunks, s.chunk(map[string]any{"reasoning_content": delta}, nil, nil))
		}
	case "response.output_item.added":
		if item, ok := event["item"].(map[string]any); ok && item["type"] == "function_call" {
			call := s.addToolCall(event, item)
			chunks = append(chunks, s.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": call.Index, "id": call.ID, "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": ""},
			}}}, nil, nil))
		}
	case "response.function_call_arguments.delta":
		delta, _ := event["delta"].(string)
		if call := s.findToolCall(event); call != nil && delta != "" {
			call.Arguments.WriteString(delta)
			chunks = append(chunks, s.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": call.Index, "function": map[string]any{"arguments": delta},
			}}}, nil, nil))
		}
	case "response.output_item.done":
		if item, ok := event["item"].(map[string]any); ok && item["type"] == "reasoning" && s.reasoning.Len() == 0 {
			if text := reasoningItemText(item); text != "" {
				s.reasoning.WriteString(text)
				chunks = append(chunks, s.chunk(map[string]any{"reasoning_content": text}, nil, nil))
			}
		}
		if item, ok := event["item"].(map[string]any); ok && item["type"] == "function_call" {
			call := s.findToolCall(event)
			if call == nil {
				call = s.addToolCall(event, item)
				arguments, _ := item["arguments"].(string)
				call.Arguments.WriteString(arguments)
				chunks = append(chunks, s.chunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": call.Index, "id": call.ID, "type": "function",
					"function": map[string]any{"name": call.Name, "arguments": arguments},
				}}}, nil, nil))
			} else if call.Arguments.Len() == 0 {
				arguments, _ := item["arguments"].(string)
				if arguments != "" {
					call.Arguments.WriteString(arguments)
					chunks = append(chunks, s.chunk(map[string]any{"tool_calls": []any{map[string]any{
						"index": call.Index, "function": map[string]any{"arguments": arguments},
					}}}, nil, nil))
				}
			}
		}
	case "response.completed", "response.incomplete":
		s.completed = true
		if response, ok := event["response"].(map[string]any); ok {
			if id, ok := response["id"].(string); ok && id != "" {
				s.id = id
			}
			if model, ok := response["model"].(string); ok && model != "" {
				s.model = model
			}
			s.readUsage(response["usage"])
		}
		if eventType == "response.incomplete" {
			s.finishReason = "length"
		} else if len(s.toolCalls) > 0 {
			s.finishReason = "tool_calls"
		} else {
			s.finishReason = "stop"
		}
		chunks = append(chunks, s.chunk(map[string]any{}, s.finishReason, s.usageMap()))
	case "response.failed", "error":
		s.failed = true
		s.failureMessage = "codex: upstream response failed"
	case "response.output_text.done", "response.function_call_arguments.done", "response.reasoning_summary_text.done", "response.content_part.added", "response.content_part.done":
	default:
	}
	return chunks, nil
}

func emitsChunk(eventType string) bool {
	switch eventType {
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_content_text.delta", "response.reasoning_text.delta", "response.output_item.added", "response.function_call_arguments.delta", "response.completed", "response.incomplete":
		return true
	default:
		return false
	}
}

func reasoningItemText(item map[string]any) string {
	for _, key := range []string{"summary", "content"} {
		parts, _ := item[key].([]any)
		var text strings.Builder
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			value, _ := part["text"].(string)
			text.WriteString(value)
		}
		if text.Len() > 0 {
			return text.String()
		}
	}
	return ""
}

func (s *streamState) addToolCall(event, item map[string]any) *toolCall {
	call := &toolCall{Index: len(s.toolCalls)}
	call.ID, _ = item["call_id"].(string)
	if call.ID == "" {
		call.ID = "call_" + randomID()
	}
	call.ItemID, _ = item["id"].(string)
	call.Name, _ = item["name"].(string)
	s.toolCalls = append(s.toolCalls, call)
	if call.ItemID != "" {
		s.toolByItem[call.ItemID] = call
	}
	if index, ok := numberAsInt(event["output_index"]); ok {
		s.toolByOutput[index] = call
	}
	return call
}

func (s *streamState) findToolCall(event map[string]any) *toolCall {
	if itemID, ok := event["item_id"].(string); ok {
		if call := s.toolByItem[itemID]; call != nil {
			return call
		}
	}
	if item, ok := event["item"].(map[string]any); ok {
		if itemID, ok := item["id"].(string); ok {
			if call := s.toolByItem[itemID]; call != nil {
				return call
			}
		}
		if callID, ok := item["call_id"].(string); ok {
			for _, call := range s.toolCalls {
				if call.ID == callID {
					return call
				}
			}
		}
	}
	if index, ok := numberAsInt(event["output_index"]); ok {
		return s.toolByOutput[index]
	}
	if len(s.toolCalls) == 1 {
		return s.toolCalls[0]
	}
	return nil
}

func (s *streamState) readUsage(value any) {
	data, _ := value.(map[string]any)
	s.usage.PromptTokens, _ = numberAsInt(first(data["input_tokens"], data["prompt_tokens"]))
	s.usage.CompletionTokens, _ = numberAsInt(first(data["output_tokens"], data["completion_tokens"]))
	s.usage.TotalTokens, _ = numberAsInt(data["total_tokens"])
	if s.usage.TotalTokens == 0 {
		s.usage.TotalTokens = s.usage.PromptTokens + s.usage.CompletionTokens
	}
	if details, ok := data["input_tokens_details"].(map[string]any); ok {
		s.usage.CachedTokens, _ = numberAsInt(details["cached_tokens"])
	}
	if details, ok := data["output_tokens_details"].(map[string]any); ok {
		s.usage.ReasoningTokens, _ = numberAsInt(details["reasoning_tokens"])
	}
	s.usage.CacheCreationInputTokens, _ = numberAsInt(data["cache_creation_input_tokens"])
}

func (s *streamState) usageMap() map[string]any {
	out := map[string]any{
		"prompt_tokens": s.usage.PromptTokens, "completion_tokens": s.usage.CompletionTokens, "total_tokens": s.usage.TotalTokens,
	}
	if s.usage.CachedTokens > 0 || s.usage.CacheCreationInputTokens > 0 {
		details := map[string]any{}
		if s.usage.CachedTokens > 0 {
			details["cached_tokens"] = s.usage.CachedTokens
		}
		if s.usage.CacheCreationInputTokens > 0 {
			details["cache_creation_tokens"] = s.usage.CacheCreationInputTokens
		}
		out["prompt_tokens_details"] = details
	}
	if s.usage.ReasoningTokens > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": s.usage.ReasoningTokens}
	}
	return out
}

func (s *streamState) chunk(delta map[string]any, finishReason any, usage map[string]any) map[string]any {
	out := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out
}

func writeSSEChunk(w io.Writer, chunk map[string]any) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func parseSSE(ctx context.Context, reader io.Reader, consume func(string, []byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var eventType string
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			eventType = ""
			return nil
		}
		payload := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		typeName := eventType
		eventType = ""
		if payload == "[DONE]" {
			return nil
		}
		return consume(typeName, []byte(payload))
	}
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("codex: read SSE stream: %w", err)
	}
	return flush()
}

func numberAsInt(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), true
	case json.Number:
		value, err := number.Int64()
		return int(value), err == nil
	case int:
		return number, true
	default:
		return 0, false
	}
}

func first(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func randomID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value)
}

func init() {
	provider.Register("codex", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return New(cfg, store)
	})
}
