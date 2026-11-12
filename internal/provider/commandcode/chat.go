package commandcode

import (
	"bytes"
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

func (p *Provider) ChatCompletion(ctx context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	model := bareModel(req.Model)
	accounts, drainErr := p.pool.AvailableForModel(p.store, model)
	if len(accounts) == 0 {
		if drainErr != nil {
			return drainErr
		}
		return fmt.Errorf("commandcode: not authenticated")
	}
	var lastErr error
	for _, account := range accounts {
		attempt := provider.NewStreamingAttemptWriter(w, req.Stream)
		lastErr = p.chatWithAccount(ctx, req, model, account, attempt)
		if lastErr == nil {
			return attempt.Commit(w)
		}
		if attempt.Committed() {
			return lastErr
		}
		if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
			return lastErr
		}
		p.pool.MarkFailed(account, model, lastErr)
	}
	return lastErr
}

func (p *Provider) chatWithAccount(ctx context.Context, req provider.ChatRequest, model string, account provider.Credentials, w http.ResponseWriter) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	key := apiKey(account)
	if key == "" {
		return fmt.Errorf("commandcode: not authenticated")
	}
	threadID := newThreadID()
	body, err := openAIToCommandCode(req, model, threadID, time.Now)
	if err != nil {
		return err
	}
	guard := provider.NewIdleGuard(p.idle, provider.StreamBudget(p.streamTimeout, req.Stream), func() { cancel(provider.ErrUpstreamStall) })
	defer guard.Stop()
	var resp *http.Response
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		up, err := http.NewRequestWithContext(ctx, http.MethodPost, p.generateURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("commandcode: create upstream request: %w", err)
		}
		for k, v := range buildCommandCodeHeaders(key, threadID, "text/event-stream") {
			up.Header[k] = v
		}
		resp, err = provider.ClientFor(p.stream, p.client, req.Stream).Do(up)
		if err != nil {
			return guard.Err(err)
		}
		if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
			provider.WarnUpstreamError(p.name, model, account.Identity(), resp.StatusCode, provider.UpstreamSnippet(resp.Body))
			resp.Body.Close()
			continue
		}
		break
	}
	if resp == nil {
		return provider.NewHTTPStatusError(http.StatusBadGateway, "commandcode: upstream unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		provider.WarnUpstreamError(p.name, model, account.Identity(), resp.StatusCode, provider.UpstreamSnippet(resp.Body))
		return provider.NewHTTPStatusError(resp.StatusCode, "commandcode: authentication failed")
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusPaymentRequired {
		provider.WarnUpstreamError(p.name, model, account.Identity(), resp.StatusCode, provider.UpstreamSnippet(resp.Body))
		return provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("commandcode: upstream status %d", resp.StatusCode))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		provider.WarnUpstreamError(p.name, model, account.Identity(), resp.StatusCode, provider.UpstreamSnippet(resp.Body))
		return provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("commandcode: upstream status %d", resp.StatusCode))
	}
	reader := guard.Wrap(resp.Body)
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		seen := false
		_, err := translateNDJSON(ctx, reader, model, account.Identity(), true, w, &seen)
		if err != nil {
			return err
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}
	return p.completeGenerate(ctx, reader, model, account, w)
}

func (p *Provider) completeGenerate(ctx context.Context, reader io.Reader, model string, account provider.Credentials, w http.ResponseWriter) error {
	state := newGenerateState(model)
	usage, err := consumeNDJSON(ctx, reader, state, nil)
	if err != nil {
		return err
	}
	usage.Account = account.Identity()
	provider.ReportUsage(ctx, usage)
	if state.failed {
		return provider.NewHTTPStatusError(http.StatusBadGateway, "commandcode: "+state.failure)
	}
	if !state.completed {
		if !state.hasEvents() {
			return provider.NewHTTPStatusError(http.StatusBadGateway, "commandcode: upstream stream closed before first payload")
		}
		return provider.NewHTTPStatusError(http.StatusBadGateway, "commandcode: incomplete SSE response")
	}
	message := map[string]any{"role": "assistant", "content": state.text.String()}
	if state.reasoning.Len() > 0 {
		message["reasoning_content"] = state.reasoning.String()
	}
	if len(state.toolCalls) > 0 {
		calls := make([]any, 0, len(state.toolCalls))
		for _, call := range state.toolCalls {
			calls = append(calls, map[string]any{
				"id": call.id, "type": "function",
				"function": map[string]any{"name": call.name, "arguments": call.args.String()},
			})
		}
		message["tool_calls"] = calls
	}
	finish := state.finishReason
	if finish == "" {
		finish = "stop"
	}
	response := map[string]any{
		"id": state.id, "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens": state.promptTokens, "completion_tokens": state.completionTokens,
			"total_tokens": state.promptTokens + state.completionTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(response)
}

func bareModel(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}
