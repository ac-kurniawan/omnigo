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
	name   string
	store  provider.CredStore
	client *http.Client
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
		timeout = 30 * time.Second
	}
	return &Provider{name: cfg.Name, store: store, client: &http.Client{Timeout: timeout, Transport: cfg.Transport}}
}

func (p *Provider) Name() string { return p.name }

func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	c, err := p.ensureFreshToken(ctx)
	if err != nil {
		return nil, err
	}
	ids, err := fetchModels(ctx, c.AccessToken, c.ProjectID)
	if err != nil && isAuthError(err) && c.RefreshToken != "" {
		// 401 or auth error from upstream -> force refresh and retry once
		if fresh, refErr := p.forceRefreshToken(ctx); refErr == nil {
			c = fresh
			ids, err = fetchModels(ctx, c.AccessToken, c.ProjectID)
		}
	}
	if err != nil {
		return nil, err
	}
	models := make([]provider.Model, 0, len(ids))
	for _, id := range ids {
		models = append(models, provider.Model{ID: id, Name: id})
	}
	return models, nil
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
	c, err := p.ensureFreshToken(ctx)
	if err != nil {
		return err
	}

	resp, err := p.sendStreamRequest(ctx, c, req)
	if err != nil && isAuthStatus(resp) && c.RefreshToken != "" {
		// 401/403 from Google -> force refresh and retry once
		if fresh, refErr := p.forceRefreshToken(ctx); refErr == nil {
			c = fresh
			resp, err = p.sendStreamRequest(ctx, c, req)
		}
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("antigravity: status %d", resp.StatusCode)
	}
	if !req.Stream {
		return p.completeToOpenAI(resp.Body, req.Model, w)
	}
	return p.streamToOpenAI(ctx, resp.Body, w)
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
	resp, err := p.client.Do(up)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp, fmt.Errorf("antigravity: status %d", resp.StatusCode)
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

// ensureFreshToken refreshes the access token when it is missing or expiring
// within 5 minutes, persisting the result through the store.
func (p *Provider) ensureFreshToken(ctx context.Context) (provider.Credentials, error) {
	c := p.store.Get()
	if c.AccessToken == "" && c.RefreshToken == "" {
		return c, fmt.Errorf("antigravity: not authenticated")
	}
	if c.AccessToken != "" && !c.ExpiresAt.IsZero() && time.Until(c.ExpiresAt) > 5*time.Minute {
		return c, nil
	}
	if c.RefreshToken == "" {
		return c, fmt.Errorf("antigravity: token expired and no refresh token")
	}
	return p.forceRefreshToken(ctx)
}

func (p *Provider) forceRefreshToken(ctx context.Context) (provider.Credentials, error) {
	c := p.store.Get()
	if c.RefreshToken == "" {
		return c, fmt.Errorf("antigravity: no refresh token")
	}
	tok, err := Refresh(ctx, c.RefreshToken)
	if err != nil {
		return c, err
	}
	c.AccessToken = tok.AccessToken
	c.ExpiresAt = tok.ExpiresAt
	if tok.RefreshToken != "" {
		c.RefreshToken = tok.RefreshToken
	}
	// Discover project if empty
	if c.ProjectID == "" {
		if pid, err := DiscoverProject(ctx, c.AccessToken); err == nil && pid != "" {
			c.ProjectID = pid
		}
	}
	if err := p.store.Put(c); err != nil {
		return c, err
	}
	return c, nil
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
