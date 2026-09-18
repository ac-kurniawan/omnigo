package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type Provider struct {
	name          string
	store         provider.CredStore
	client        *http.Client
	stream        *http.Client
	idle          time.Duration
	streamTimeout time.Duration
	pool          provider.AccountPool
	tokens        sync.Map
}

var sseBufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 64*1024)
		return &b
	},
}

func New(cfg provider.Config, store provider.CredStore) provider.Provider {
	if base := strings.TrimRight(cfg.BaseURL, "/"); base != "" {
		baseURL = base
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	client := &http.Client{Timeout: timeout, Transport: cfg.Transport}
	return &Provider{
		name:          cfg.Name,
		store:         store,
		client:        client,
		stream:        provider.StreamClient(client),
		idle:          timeout,
		streamTimeout: cfg.StreamTimeout,
	}
}

func (p *Provider) Name() string { return p.name }

func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	accounts, drainErr := p.pool.AvailableWithError(p.store)
	lastErr := drainErr
	for _, account := range accounts {
		tokens := p.tokenManager(account)
		c, err := tokens.EnsureFreshToken(ctx)
		if err == nil {
			var ids []string
			ids, err = fetchModels(ctx, c.AccessToken, c.ProjectID)
			if err != nil && isAuthError(err) && c.RefreshToken != "" {
				if fresh, refErr := tokens.ForceRefreshToken(ctx, c.AccessToken); refErr == nil {
					c = fresh
					ids, err = fetchModels(ctx, c.AccessToken, c.ProjectID)
				}
			}
			if err == nil {
				models := make([]provider.Model, 0, len(ids))
				for _, id := range ids {
					models = append(models, provider.Model{ID: id, Name: id})
				}
				return models, nil
			}
		}
		lastErr = err
		p.pool.MarkFailed(account, err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("antigravity: not authenticated")
	}
	return nil, lastErr
}

func (p *Provider) Test(ctx context.Context) provider.TestResult {
	start := time.Now()
	_, err := p.Models(ctx)
	res := provider.TestResult{LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}

func (p *Provider) ChatCompletion(ctx context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
	accounts, drainErr := p.pool.AvailableWithError(p.store)
	if len(accounts) == 0 {
		if drainErr != nil {
			return drainErr
		}
		return fmt.Errorf("antigravity: not authenticated")
	}
	var lastErr error
	for _, account := range accounts {
		attempt := provider.NewStreamingAttemptWriter(w, req.Stream)
		lastErr = p.chatWithAccount(ctx, req, account, attempt)
		if lastErr == nil {
			return attempt.Commit(w)
		}
		if attempt.Committed() {
			return lastErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.pool.MarkFailed(account, lastErr)
	}
	return lastErr
}

func (p *Provider) chatWithAccount(ctx context.Context, req provider.ChatRequest, account provider.Credentials, w http.ResponseWriter) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	tokens := p.tokenManager(account)
	c, err := tokens.EnsureFreshToken(ctx)
	if err != nil {
		return err
	}
	// Armed after token refresh so the idle window covers the generation
	// exchange only, not credential work.
	guard := provider.NewIdleGuard(p.idle, provider.StreamBudget(p.streamTimeout, req.Stream), func() { cancel(provider.ErrUpstreamStall) })
	defer guard.Stop()
	resp, err := p.sendStreamRequest(ctx, c, req)
	if err != nil && isAuthStatus(resp) && c.RefreshToken != "" {
		if resp != nil {
			resp.Body.Close()
		}
		if fresh, refErr := tokens.ForceRefreshToken(ctx, c.AccessToken); refErr == nil {
			c = fresh
			resp, err = p.sendStreamRequest(ctx, c, req)
		} else {
			return refErr
		}
	}
	if err != nil {
		return guard.Err(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return newRateLimitError(resp.Header)
	}
	if resp.StatusCode != http.StatusOK {
		return provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("antigravity: status %d", resp.StatusCode))
	}
	body := guard.Wrap(resp.Body)
	if !req.Stream {
		return p.completeToOpenAI(body, req.Model, w)
	}
	return p.streamToOpenAI(ctx, body, w)
}

func (p *Provider) sendStreamRequest(ctx context.Context, c provider.Credentials, req provider.ChatRequest) (*http.Response, error) {
	env, err := ToEnvelope(c.ProjectID, req.Model, req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1internal:streamGenerateContent?alt=sse", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept", "text/event-stream")
	up.Header.Set("User-Agent", antigravityUserAgent)
	up.Header.Set("X-Goog-Api-Client", antigravityGoogAPI)
	up.Header.Set("Authorization", "Bearer "+c.AccessToken)
	resp, err := provider.ClientFor(p.stream, p.client, req.Stream).Do(up)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp, provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("antigravity: status %d", resp.StatusCode))
	}
	return resp, nil
}

func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "status 401") || strings.Contains(msg, "status 403")
}

func isAuthStatus(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
}

func (p *Provider) tokenManager(account provider.Credentials) *TokenManager {
	key := account.Identity()
	if key == "" {
		key = account.RefreshToken
	}
	manager, _ := p.tokens.LoadOrStore(key, NewTokenManager(provider.ScopedStore(p.store, account)))
	return manager.(*TokenManager)
}

func (p *Provider) completeToOpenAI(r io.Reader, model string, w http.ResponseWriter) error {
	text, err := aggregateSSE(r)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-" + newRequestID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": text,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	})
}

func aggregateSSE(r io.Reader) (string, error) {
	var text strings.Builder
	scanner := bufio.NewScanner(r)
	bufp := sseBufferPool.Get().(*[]byte)
	defer sseBufferPool.Put(bufp)
	scanner.Buffer((*bufp)[:0], 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		chunk, err := geminiChunkText([]byte(line))
		if err != nil {
			return "", err
		}
		text.WriteString(chunk)
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return text.String(), nil
}

func (p *Provider) streamToOpenAI(ctx context.Context, r io.Reader, w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/event-stream")
	scanner := bufio.NewScanner(r)
	bufp := sseBufferPool.Get().(*[]byte)
	defer sseBufferPool.Put(bufp)
	scanner.Buffer((*bufp)[:0], 1024*1024)
	flusher, _ := w.(http.Flusher)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		out, err := TranslateSSE([]byte(line))
		if err != nil || out == nil {
			continue
		}
		if _, err := w.Write(out); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if _, err := w.Write([]byte("data: [DONE]\n\n")); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func init() {
	provider.Register("antigravity", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return New(cfg, store)
	})
}
