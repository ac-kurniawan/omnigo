package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
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
		baseURL = normalizeBaseURL(base)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if cfg.Transport != nil {
		SetHTTPTransport(cfg.Transport)
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

// normalizeBaseURL redirects the legacy cloudcode-pa host to the daily host
// the official Antigravity CLI uses. Existing user configs seeded with the
// legacy host otherwise keep hitting the endpoint that 429s consumer accounts.
func normalizeBaseURL(base string) string {
	const legacy = "https://cloudcode-pa.googleapis.com"
	if strings.TrimRight(base, "/") == legacy {
		return defaultBaseURL
	}
	return base
}

func (p *Provider) Name() string { return p.name }

// Client returns the streaming client so tests can prove it stays distinct
// from the bounded unary client.
func (p *Provider) Client() *http.Client { return p.stream }

func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	accounts, drainErr := p.pool.AvailableForModel(p.store, "")
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
		p.pool.MarkFailed(account, "", err)
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
	accounts, drainErr := p.pool.AvailableForModel(p.store, req.Model)
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
		p.pool.MarkFailed(account, req.Model, lastErr)
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
		snippet := readUpstreamSnippet(resp.Body)
		err := newRateLimitError(resp.Header, snippet)
		log.Printf("[rate-limited] provider=%q account=%q model=%q retry-after=%s cooldown=%s upstream=%q",
			p.name, rateLimitAccountLabel(account), req.Model, retryAfterLabel(resp.Header), err.(*rateLimitError).Cooldown().Round(time.Second), err.(*rateLimitError).detail)
		return err
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

// maxUpstreamSnippetBytes caps how much of a 429 body is read for diagnostics.
// The error text is a small JSON status; a larger read only risks a hostile
// upstream filling memory on an error path.
const maxUpstreamSnippetBytes = 4096

// readUpstreamSnippet drains a bounded prefix of an error body for logging and
// for the client-facing rate-limit message.
func readUpstreamSnippet(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, maxUpstreamSnippetBytes))
	if err != nil {
		return ""
	}
	return string(raw)
}

func retryAfterLabel(headers http.Header) string {
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
		return (time.Duration(seconds) * time.Second).String()
	}
	if raw == "" {
		return "absent"
	}
	return "invalid"
}

func rateLimitAccountLabel(account provider.Credentials) string {
	if account.AccountID != "" {
		return account.AccountID
	}
	if account.UserID != "" {
		return account.UserID
	}
	return "configured-account"
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
	done, err := aggregateSSE(r)
	if err != nil {
		return err
	}
	message := map[string]any{"role": "assistant", "content": done.text}
	if done.thought != "" {
		message["reasoning_content"] = done.thought
	}
	if len(done.calls) > 0 {
		message["tool_calls"] = done.calls
	}
	usage := done.usage
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-" + newRequestID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": done.reason,
		}},
		"usage": usage,
	})
}

// generation is one finished Gemini stream, folded into the OpenAI message
// shape. reason is already in the OpenAI vocabulary.
type generation struct {
	text    string
	thought string
	calls   []any
	reason  string
	usage   map[string]any
}

func aggregateSSE(r io.Reader) (generation, error) {
	var text, thought strings.Builder
	var calls []any
	var done generation
	finished := false
	err := scanSSE(r, func(body geminiBody) error {
		chunkText, chunkThought, chunkCalls := body.content(len(calls))
		text.WriteString(chunkText)
		thought.WriteString(chunkThought)
		calls = append(calls, chunkCalls...)
		if reason := body.finishReason(); reason != "" {
			finished = true
			done.reason = finishReason(reason, len(calls))
		}
		if usage := body.usageMap(); usage != nil {
			done.usage = usage
		}
		return nil
	})
	if err != nil {
		return generation{}, err
	}
	// Same cut-off as the streaming path: a feed that closes without a finish
	// reason is a truncated answer, not a short completion.
	if !finished {
		return generation{}, errors.New("antigravity: incomplete SSE response")
	}
	done.text = text.String()
	done.thought = thought.String()
	done.calls = calls
	return done, nil
}

func (p *Provider) streamToOpenAI(ctx context.Context, r io.Reader, w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	finished := false
	calls := 0
	err := scanSSE(r, func(body geminiBody) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		out, done, callsInFrame, err := frameToSSE(body, calls)
		if err != nil {
			return err
		}
		calls += callsInFrame
		if done {
			finished = true
		}
		if len(out) == 0 {
			return nil
		}
		if _, err := w.Write(out); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	// A stream that ends without a finish reason was cut off: the upstream
	// closed before the generation completed. Closing it with [DONE] would make
	// the client treat the truncated answer as final, so fail the attempt
	// instead and let the combo fail over.
	if !finished {
		return errors.New("antigravity: incomplete SSE response")
	}
	if _, err := w.Write([]byte("data: [DONE]\n\n")); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// scanSSE walks the data lines of a Gemini SSE feed and hands each decoded
// frame to consume. A line that is not a data frame is skipped. The decode
// happens once per frame.
func scanSSE(r io.Reader, consume func(geminiBody) error) error {
	scanner := bufio.NewScanner(r)
	bufp := sseBufferPool.Get().(*[]byte)
	defer sseBufferPool.Put(bufp)
	scanner.Buffer((*bufp)[:0], 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		body, err := parseGeminiFrame([]byte(line))
		if err != nil {
			return err
		}
		if err := consume(body); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func init() {
	provider.Register("antigravity", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return New(cfg, store)
	})
}
