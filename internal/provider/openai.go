package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var streamBufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 4096)
		return &b
	},
}

type openAIProvider struct {
	name    string
	baseURL string
	store   CredStore
	client  *http.Client
	stream  *http.Client
	idle    time.Duration
}

func NewOpenAI(cfg Config, store CredStore) Provider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout, Transport: cfg.Transport}
	return &openAIProvider{
		name:    cfg.Name,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		store:   store,
		client:  client,
		stream:  StreamClient(client),
		idle:    timeout,
	}
}

func (p *openAIProvider) Name() string { return p.name }

func (p *openAIProvider) Models(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	p.authorize(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: status %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(out.Data))
	for _, m := range out.Data {
		models = append(models, Model{ID: m.ID, Name: m.ID})
	}
	return models, nil
}

func (p *openAIProvider) Test(ctx context.Context) TestResult {
	start := time.Now()
	_, err := p.Models(ctx)
	res := TestResult{LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}

func (p *openAIProvider) ChatCompletion(ctx context.Context, req ChatRequest, w http.ResponseWriter) error {
	body, err := req.Body()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	guard := NewIdleGuard(p.idle, func() { cancel(ErrUpstreamStall) })
	defer guard.Stop()
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	up.Header.Set("Content-Type", "application/json")
	p.authorize(up)
	resp, err := ClientFor(p.stream, p.client, req.Stream).Do(up)
	if err != nil {
		return guard.Err(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("upstream status %d", resp.StatusCode))
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	bufp := streamBufferPool.Get().(*[]byte)
	defer streamBufferPool.Put(bufp)
	buf := *bufp
	reader := guard.Wrap(resp.Body)
	for {
		n, rErr := reader.Read(buf)
		if n > 0 {
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				return wErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rErr != nil {
			if rErr == io.EOF {
				break
			}
			return rErr
		}
	}
	return nil
}

// allowedResponseHeaders lists the upstream headers worth forwarding to a
// gateway client. Everything else stays upstream: Set-Cookie and
// WWW-Authenticate describe a session the client does not own, and internal
// routing/debug headers disclose infrastructure detail. Content-Type and
// Cache-Control keep SSE clients behaving correctly.
var allowedResponseHeaders = map[string]bool{
	"Content-Type":     true,
	"Cache-Control":    true,
	"Content-Encoding": true,
	"Retry-After":      true,
}

func copyResponseHeaders(destination, source http.Header) {
	for key, values := range source {
		if !allowedResponseHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, v := range values {
			destination.Add(key, v)
		}
	}
}

func (p *openAIProvider) authorize(req *http.Request) {
	if c := p.store.Get(); c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
}

func init() {
	Register("openai", func(cfg Config, store CredStore) Provider { return NewOpenAI(cfg, store) })
}
