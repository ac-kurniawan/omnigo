package commandcode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/provider"
)

const (
	defaultGenerateURL = "https://api.commandcode.ai/alpha/generate"
	defaultModelsURL   = "https://api.commandcode.ai/provider/v1/models?view=client"

	commandCodeVersion = "1.88.5"
	defaultUserAgent   = "cli"
	defaultProbeModel  = "zai-org/GLM-5.2"
)

func init() {
	provider.Register("commandcode", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return New(cfg, store)
	})
}

// DefaultModels is the offline fallback catalog. The live catalog is fetched
// from the vendor endpoint; this list only serves when that fetch fails.
var DefaultModels = []provider.Model{
	{ID: "deepseek/deepseek-v4-pro", Name: "deepseek/deepseek-v4-pro"},
	{ID: "deepseek/deepseek-v4-flash", Name: "deepseek/deepseek-v4-flash"},
	{ID: "moonshotai/Kimi-K3.1-Pro", Name: "moonshotai/Kimi-K3.1-Pro"},
	{ID: "moonshotai/Kimi-K2.7-Code", Name: "moonshotai/Kimi-K2.7-Code"},
	{ID: "moonshotai/Kimi-K2.6", Name: "moonshotai/Kimi-K2.6"},
	{ID: "moonshotai/Kimi-K2.5", Name: "moonshotai/Kimi-K2.5"},
	{ID: "zai-org/GLM-5.2", Name: "zai-org/GLM-5.2"},
	{ID: "zai-org/GLM-5.1", Name: "zai-org/GLM-5.1"},
	{ID: "zai-org/GLM-5", Name: "zai-org/GLM-5"},
	{ID: "MiniMaxAI/MiniMax-M3", Name: "MiniMaxAI/MiniMax-M3"},
	{ID: "MiniMaxAI/MiniMax-M2.7", Name: "MiniMaxAI/MiniMax-M2.7"},
	{ID: "xiaomi/mimo-v2.5-pro", Name: "xiaomi/mimo-v2.5-pro"},
	{ID: "Qwen/Qwen3.7-Max", Name: "Qwen/Qwen3.7-Max"},
	{ID: "Qwen/Qwen3.6-Plus", Name: "Qwen/Qwen3.6-Plus"},
	{ID: "stepfun/Step-3.7-Flash", Name: "stepfun/Step-3.7-Flash"},
	{ID: "nvidia/nemotron-3-ultra-550b-a55b", Name: "nvidia/nemotron-3-ultra-550b-a55b"},
	{ID: "google/gemini-4-carbon", Name: "google/gemini-4-carbon"},
	{ID: "claude-fable-5-2", Name: "claude-fable-5-2"},
}

type Provider struct {
	name      string
	generate  string
	models    string
	cfgModels []string
	store     provider.CredStore
	pool      provider.AccountPool
	client    *http.Client
	stream    *http.Client
	idle      time.Duration
	budget    time.Duration

	projectSlugOnce sync.Once
	projectSlug     string
}

func New(cfg provider.Config, store provider.CredStore) provider.Provider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	generate := strings.TrimSpace(cfg.BaseURL)
	if generate == "" {
		generate = defaultGenerateURL
	}
	client := &http.Client{Timeout: timeout, Transport: cfg.Transport}
	return &Provider{
		name:      cfg.Name,
		generate:  generate,
		models:    modelsURLFor(generate),
		cfgModels: cfg.Models,
		store:     store,
		client:    client,
		stream:    provider.StreamClient(client),
		idle:      timeout,
		budget:    cfg.StreamTimeout,
	}
}

// modelsURLFor derives the live catalog URL from the configured generate
// endpoint host, so a self-hosted or test endpoint is honored.
func modelsURLFor(generate string) string {
	parsed, err := url.Parse(generate)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return defaultModelsURL
	}
	return parsed.Scheme + "://" + parsed.Host + "/provider/v1/models?view=client"
}

func (p *Provider) Name() string { return p.name }

func (p *Provider) credential() (provider.Credentials, bool) {
	creds := p.store.Get()
	key := creds.APIKey
	if key == "" {
		key = creds.AccessToken
	}
	return creds, key != ""
}

func (p *Provider) ChatCompletion(ctx context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
	accounts, _ := p.pool.AvailableForModel(p.store, req.Model)
	if len(accounts) == 0 {
		if creds, ok := p.credential(); ok {
			accounts = []provider.Credentials{creds}
		}
	}
	if len(accounts) == 0 {
		return provider.NewHTTPStatusError(http.StatusUnauthorized, "commandcode: missing api key")
	}

	sessionID := sessionID(ctx, req)
	body, err := buildEnvelope(req, sessionID)
	if err != nil {
		return err
	}

	var lastErr error
	for _, account := range accounts {
		key := account.APIKey
		if key == "" {
			key = account.AccessToken
		}
		if key == "" {
			continue
		}

		attempt := provider.NewStreamingAttemptWriter(w, req.Stream)
		lastErr = p.chatWithKey(ctx, req, body, sessionID, key, attempt)
		if lastErr == nil {
			return attempt.Commit(w)
		}
		if attempt.Committed() {
			return lastErr
		}
		if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
			return lastErr
		}
		p.pool.MarkFailed(account, req.Model, lastErr)
	}
	if lastErr == nil {
		lastErr = provider.NewHTTPStatusError(http.StatusUnauthorized, "commandcode: missing api key")
	}
	return lastErr
}

func (p *Provider) chatWithKey(ctx context.Context, req provider.ChatRequest, body []byte, sessionID, key string, w http.ResponseWriter) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	guard := provider.NewIdleGuard(p.idle, provider.StreamBudget(p.budget, req.Stream), func() { cancel(provider.ErrUpstreamStall) })
	defer guard.Stop()

	up, err := http.NewRequestWithContext(ctx, http.MethodPost, p.generate, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("commandcode: create request: %w", err)
	}
	p.setHeaders(up, key, sessionID, req.Stream)

	resp, err := provider.ClientFor(p.stream, p.client, req.Stream).Do(up)
	if err != nil {
		return guard.Err(fmt.Errorf("commandcode: forward request: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet := provider.UpstreamSnippet(resp.Body)
		provider.WarnUpstreamError(p.name, req.Model, "", resp.StatusCode, snippet)
		return provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("commandcode: upstream status %d", resp.StatusCode))
	}

	reader := guard.Wrap(resp.Body)
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		return p.translateStream(ctx, reader, req.Model, key, w)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return p.translateComplete(ctx, reader, req.Model, key, w)
}

func (p *Provider) setHeaders(req *http.Request, apiKey, sessionID string, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("x-command-code-version", commandCodeVersion)
	req.Header.Set("x-cli-environment", "production")
	req.Header.Set("x-cli-surface", "cli")
	req.Header.Set("x-taste-learning", "false")
	req.Header.Set("x-project-slug", p.slug())
	req.Header.Set("x-session-id", sessionID)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
}

// slug mirrors the CLI's x-project-slug: a lowercased, dash-separated working
// directory. The gateway has no client workspace, so its own cwd stands in.
func (p *Provider) slug() string {
	p.projectSlugOnce.Do(func() {
		p.projectSlug = slugify(p.name)
	})
	return p.projectSlug
}

func slugify(value string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(value) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// sessionID is conversation-stable so the upstream thread survives turns. It
// prefers an explicit session field in the request, then the gateway caller.
func sessionID(ctx context.Context, req provider.ChatRequest) string {
	var key string
	for _, field := range []string{"session_id", "prompt_cache_key", "conversation_id", "threadId"} {
		if value, ok := req.Parsed[field].(string); ok && value != "" {
			key = value
			break
		}
	}
	if key == "" {
		if caller := auth.CallerFrom(ctx); caller != nil {
			key = caller.ID()
		}
	}
	if key == "" {
		var buf [16]byte
		if _, err := rand.Read(buf[:]); err == nil {
			return uuidFrom(hex.EncodeToString(buf[:]))
		}
		return "00000000-0000-4000-8000-000000000000"
	}
	sum := sha256.Sum256([]byte(key))
	return uuidFrom(hex.EncodeToString(sum[:16]))
}

// uuidFrom renders 32 hex characters as a UUID-shaped id, which is the format
// the upstream session header already accepts.
func uuidFrom(h string) string {
	if len(h) < 32 {
		return "00000000-0000-4000-8000-000000000000"
	}
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-8" + h[17:20] + "-" + h[20:32]
}

func (p *Provider) translateStream(ctx context.Context, reader io.Reader, model, account string, w http.ResponseWriter) error {
	state := newState(model)
	flusher, _ := w.(http.Flusher)
	err := scanEvents(reader, func(event map[string]any) error {
		chunks, err := state.consume(event)
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
	if !state.finished {
		return provider.NewHTTPStatusError(http.StatusBadGateway, "commandcode: incomplete stream")
	}
	provider.ReportUsage(ctx, state.usage.tokenUsage(account))
	_, err = io.WriteString(w, "data: [DONE]\n\n")
	return err
}

func (p *Provider) translateComplete(ctx context.Context, reader io.Reader, model, account string, w http.ResponseWriter) error {
	state := newState(model)
	if err := scanEvents(reader, func(event map[string]any) error {
		_, err := state.consume(event)
		return err
	}); err != nil {
		return err
	}
	if !state.finished {
		return provider.NewHTTPStatusError(http.StatusBadGateway, "commandcode: incomplete stream")
	}
	provider.ReportUsage(ctx, state.usage.tokenUsage(account))
	return writeCompletion(w, state)
}

func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	models, err := p.fetchModels(ctx)
	if err != nil {
		return cloneModels(DefaultModels), nil
	}
	return models, nil
}

// fetchModels reads the live catalog. The catalog endpoint is public, so it
// proves reachability but not the credential; Test probes an authenticated
// endpoint on top of it.
func (p *Provider) fetchModels(ctx context.Context) ([]provider.Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.models, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("x-command-code-version", commandCodeVersion)
	req.Header.Set("x-cli-environment", "production")
	req.Header.Set("x-cli-surface", "cli")
	req.Header.Set("x-taste-learning", "false")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("commandcode: models status %d", resp.StatusCode)
	}

	var catalog struct {
		Data []struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			Hidden          bool   `json:"hidden"`
			ReasoningEffort []any  `json:"reasoningEfforts"`
			Display         struct {
				Label string `json:"label"`
			} `json:"display"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return nil, err
	}
	models := make([]provider.Model, 0, len(catalog.Data))
	for _, m := range catalog.Data {
		if m.ID == "" || m.Hidden {
			continue
		}
		name := m.Display.Label
		if name == "" {
			name = m.Name
		}
		if name == "" {
			name = m.ID
		}
		models = append(models, provider.Model{ID: m.ID, Name: name})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("commandcode: empty model catalog")
	}
	return models, nil
}

func cloneModels(models []provider.Model) []provider.Model {
	return append([]provider.Model(nil), models...)
}

func (p *Provider) Test(ctx context.Context) provider.TestResult {
	start := time.Now()
	res := provider.TestResult{}
	creds, ok := p.credential()
	if !ok {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = "no API key configured for commandcode provider"
		return res
	}
	key := creds.APIKey
	if key == "" {
		key = creds.AccessToken
	}
	if err := p.probeCredential(ctx, key); err != nil {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = err.Error()
		return res
	}
	res.OK = true
	res.LatencyMS = time.Since(start).Milliseconds()
	return res
}

// probeCredential sends a minimal authenticated generation so a revoked or
// mistyped key fails the dashboard test instead of silently passing on the
// public catalog.
func (p *Provider) probeCredential(ctx context.Context, key string) error {
	// The upstream validates memory/config on every call, so the probe reuses
	// the real envelope rather than a hand-rolled minimal body.
	envelope, err := buildEnvelope(provider.ChatRequest{
		Model: p.probeModel(),
		Parsed: map[string]any{
			"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
			"max_tokens": float64(16),
		},
	}, "00000000-0000-4000-8000-000000000000")
	if err != nil {
		return err
	}
	body := envelope
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.generate, bytes.NewReader(body))
	if err != nil {
		return err
	}
	p.setHeaders(req, key, "00000000-0000-4000-8000-000000000000", true)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("commandcode: test status %d", resp.StatusCode))
	}
	return nil
}

// probeModel prefers a configured model and falls back to one that live
// traffic already accepted, so Test stays a credential check either way.
func (p *Provider) probeModel() string {
	for _, m := range p.cfgModels {
		if m = strings.TrimSpace(m); m != "" {
			return m
		}
	}
	return defaultProbeModel
}

// scanEvents reads one JSON event per line. The upstream emits AI SDK v5
// NDJSON; a "data:" prefix is tolerated so the same reader also accepts an
// SSE-framed variant.
func scanEvents(reader io.Reader, consume func(map[string]any) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 50*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "data:")
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if err := consume(event); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func writeSSEChunk(w io.Writer, chunk map[string]any) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}
