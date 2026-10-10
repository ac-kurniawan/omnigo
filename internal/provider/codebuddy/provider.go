package codebuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// NeutralSystemPrompt replaces agent-identity system prompts that the
// upstream content filter rejects.
const NeutralSystemPrompt = "You are a helpful AI assistant that helps with software engineering tasks."

const DefaultUserAgent = "CLI/2.108.1 CodeBuddy/2.108.1"
const IntlUserAgent = "IDE/2.108.1 CodeBuddy/2.108.1"
const IntlSystemPrompt = "You are CodeBuddy Code."

// EdgeOneAnycastIP is the Tencent EdgeOne anycast IP used when public DNS
// sinkholes www.codebuddy.ai to 0.0.0.1 or fails to resolve.
const EdgeOneAnycastIP = "43.170.214.92"
const CodeBuddyIntlHost = "www.codebuddy.ai"

var agentSystemPromptPattern = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`you are claude code`,
	`claude.?code.+official.+cli`,
	`anthropic.+official.+cli`,
	`you are (?:cursor|windsurf|cline|aider|continue|copilot|cody)`,
	`you are an? (?:ai )?(?:coding |code )?agent`,
	`cc_entrypoint\s*=\s*(?:cli|vscode|jetbrains|gui)`,
	`claude.?code.+issues`,
	`give feedback.+claude.?code`,
	`you are .{0,30}(?:powerful )?ai agent`,
	`orchestration capabilities`,
	`OhMyOpenCode`,
	`<agent-identity>`,
	`<Role>`,
	`<Behavior_Instructions>`,
}, "|"))

type codebuddyProvider struct {
	name          string
	baseURL       string
	store         provider.CredStore
	pool          provider.AccountPool
	client        *http.Client
	stream        *http.Client
	idle          time.Duration
	streamTimeout time.Duration
	intl          bool
}

const DefaultCNBaseURL = "https://copilot.tencent.com/v2"
const DefaultIntlBaseURL = "https://www.codebuddy.ai/v2"

func init() {
	provider.Register("codebuddy", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return New(cfg, store, false)
	})
	provider.Register("codebuddy-cn", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return New(cfg, store, false)
	})
	provider.Register("codebuddy-intl", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return New(cfg, store, true)
	})
}

// dialContextFunc is a function type for dialing network connections.
type dialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// newCodebuddyFallbackDialer wraps dialer function to intercept connections to www.codebuddy.ai.
// If DNS resolution for www.codebuddy.ai yields 0.0.0.1 or fails, it connects to Tencent EdgeOne Anycast IP.
func newCodebuddyFallbackDialer(baseDial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return baseDial(ctx, network, addr)
		}

		if host != CodeBuddyIntlHost {
			return baseDial(ctx, network, addr)
		}

		// Perform DNS lookup for www.codebuddy.ai
		ips, lookupErr := net.DefaultResolver.LookupIP(ctx, "ip", host)
		useFallback := false
		if lookupErr != nil || len(ips) == 0 {
			useFallback = true
		} else {
			// Check if all resolved IPs are sinkholed (e.g. 0.0.0.1 or 0.0.0.0)
			allSinkholed := true
			for _, ip := range ips {
				ipStr := ip.String()
				if ipStr != "0.0.0.1" && ipStr != "0.0.0.0" {
					allSinkholed = false
					break
				}
			}
			if allSinkholed {
				useFallback = true
			}
		}

		if useFallback {
			return baseDial(ctx, network, net.JoinHostPort(EdgeOneAnycastIP, port))
		}

		// Try standard dial first, fallback on dial failure
		conn, err := baseDial(ctx, network, addr)
		if err != nil {
			return baseDial(ctx, network, net.JoinHostPort(EdgeOneAnycastIP, port))
		}
		return conn, nil
	}
}

// wrapTransportWithDNSFallback ensures HTTP transport has DNS fallback for www.codebuddy.ai.
func wrapTransportWithDNSFallback(base http.RoundTripper) http.RoundTripper {
	var tr *http.Transport
	if base == nil {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	} else if existingTr, ok := base.(*http.Transport); ok {
		tr = existingTr.Clone()
	} else {
		// Non-*http.Transport RoundTripper passed (e.g. in tests)
		return base
	}

	netDialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	dialContext := tr.DialContext
	if dialContext == nil {
		dialContext = netDialer.DialContext
	}

	tr.DialContext = newCodebuddyFallbackDialer(dialContext)
	return tr
}

// New returns a CodeBuddy provider for CN or Intl.
func New(cfg provider.Config, store provider.CredStore, forceIntl bool) provider.Provider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	transport := cfg.Transport
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		if forceIntl || strings.Contains(cfg.Name, "intl") {
			base = DefaultIntlBaseURL
		} else {
			base = DefaultCNBaseURL
		}
	}
	isIntl := forceIntl || strings.Contains(base, "codebuddy.ai") || strings.Contains(cfg.Name, "intl")
	if isIntl {
		transport = wrapTransportWithDNSFallback(transport)
	}

	client := &http.Client{Timeout: timeout, Transport: transport}
	return &codebuddyProvider{
		name:          cfg.Name,
		baseURL:       base,
		store:         store,
		client:        client,
		stream:        provider.StreamClient(client),
		idle:          timeout,
		streamTimeout: cfg.StreamTimeout,
		intl:          isIntl,
	}
}

func (p *codebuddyProvider) Name() string { return p.name }

func (p *codebuddyProvider) isIntl() bool {
	return p.intl || strings.Contains(p.baseURL, "codebuddy.ai") || strings.Contains(p.name, "intl")
}

// transformRequest applies CodeBuddy's quirks to an outgoing request:
// forces stream=true (upstream rejects non-streaming with 11101),
// neutralizes agent-identity system prompts that Tencent's content
// filter rejects, and mirrors the CLI's reasoning parameters
// (reasoning_summary: "auto") so reasoning surfaces.
// For CodeBuddy Intl, leading system prompt "You are CodeBuddy Code."
// is enforced, and user content is wrapped into typed blocks.
func (p *codebuddyProvider) transformRequest(req provider.ChatRequest) ([]byte, error) {
	// Re-encode from Parsed if available so we can edit fields; fall back
	// to decoding Raw.
	var payload map[string]any
	if req.Parsed != nil {
		// Copy without the internal cache slot.
		payload = make(map[string]any, len(req.Parsed))
		for k, v := range req.Parsed {
			if k == provider.OpenAIBodyCacheKey() {
				continue
			}
			payload[k] = v
		}
	} else if len(req.Raw) > 0 {
		if err := json.Unmarshal(req.Raw, &payload); err != nil {
			return nil, fmt.Errorf("decode request body: %w", err)
		}
	} else {
		payload = map[string]any{}
	}

	payload["model"] = req.Model
	payload["stream"] = true

	if p.isIntl() {
		// CodeBuddy Intl rejects plain OpenAI shape (error 11101): needs
		// leading system prompt + user content as typed blocks ([{type:"text",text:"..."}]).
		var newMessages []map[string]any
		newMessages = append(newMessages, map[string]any{
			"role":    "system",
			"content": IntlSystemPrompt,
		})

		if messages, ok := payload["messages"].([]any); ok {
			for _, m := range messages {
				msg, ok := m.(map[string]any)
				if !ok {
					continue
				}
				role, _ := msg["role"].(string)
				if role == "system" || role == "developer" {
					continue
				}
				if role == "user" {
					if contentStr, ok := msg["content"].(string); ok {
						newMessages = append(newMessages, map[string]any{
							"role": role,
							"content": []map[string]any{
								{"type": "text", "text": contentStr},
							},
						})
						continue
					}
				}
				newMessages = append(newMessages, msg)
			}
		}
		payload["messages"] = newMessages
	} else {
		// CodeBuddy CN: Neutralize agent system prompts (>2000 chars or identity markers).
		if messages, ok := payload["messages"].([]any); ok {
			for i, m := range messages {
				message, ok := m.(map[string]any)
				if !ok {
					continue
				}
				if role, _ := message["role"].(string); role != "system" && role != "developer" {
					continue
				}
				text := flattenContent(message["content"])
				if text == "" {
					continue
				}
				if len(text) > 2000 || agentSystemPromptPattern.MatchString(text) {
					message["role"] = "system"
					message["content"] = NeutralSystemPrompt
					messages[i] = message
				}
			}
		}
	}

	// Mirror the CLI reasoning params: CodeBuddy surfaces reasoning only
	// with reasoning_summary:"auto". "none"/"off" means omit both.
	if eff, ok := payload["reasoning_effort"].(string); ok {
		switch eff {
		case "none", "off":
			delete(payload, "reasoning_effort")
			delete(payload, "reasoning_summary")
		default:
			payload["reasoning_summary"] = "auto"
		}
	} else {
		delete(payload, "reasoning_summary")
	}

	return json.Marshal(payload)
}

// flattenContent flattens a message content that may be a string or
// typed blocks ([{type:"text",text:"..."}]) into plain text.
func flattenContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, block := range v {
			if m, ok := block.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					sb.WriteString(text)
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

func (p *codebuddyProvider) apiKey() (string, bool) {
	if p.store == nil {
		return "", false
	}
	creds := p.store.Get()
	if creds.APIKey != "" {
		return creds.APIKey, true
	}
	if creds.AccessToken != "" {
		return creds.AccessToken, true
	}
	return "", false
}

func (p *codebuddyProvider) buildHeaders(req *http.Request, apiKey string, streaming bool) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if p.isIntl() {
		req.Header.Set("User-Agent", IntlUserAgent)
		req.Header.Set("X-Domain", "www.codebuddy.ai")
		req.Header.Set("X-IDE-Type", "IDE")
		req.Header.Set("X-IDE-Name", "IDE")
	} else {
		req.Header.Set("User-Agent", DefaultUserAgent)
		req.Header.Set("X-IDE-Type", "CLI")
		req.Header.Set("X-IDE-Name", "CLI")
	}
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("x-requested-with", "XMLHttpRequest")
	req.Header.Set("x-codebuddy-request", "1")
	if streaming {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
}

func (p *codebuddyProvider) ChatCompletion(ctx context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
	accounts, _ := p.pool.AvailableForModel(p.store, req.Model)
	if len(accounts) == 0 {
		return errors.New("no API key configured for codebuddy provider")
	}

	body, err := p.transformRequest(req)
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
		lastErr = p.chatWithKey(ctx, req, body, key, attempt)
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

	return lastErr
}

func (p *codebuddyProvider) chatWithKey(ctx context.Context, req provider.ChatRequest, body []byte, apiKey string, w http.ResponseWriter) error {
	endpoint := p.baseURL + "/chat/completions"
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	guard := provider.NewIdleGuard(p.idle, provider.StreamBudget(p.streamTimeout, req.Stream), func() { cancel(provider.ErrUpstreamStall) })
	defer guard.Stop()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	p.buildHeaders(httpReq, apiKey, true) // upstream always streams

	client := provider.ClientFor(p.stream, p.client, true)
	resp, err := client.Do(httpReq)
	if err != nil {
		return guard.Err(fmt.Errorf("forward request to codebuddy: %w", err))
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return guard.Err(fmt.Errorf("read codebuddy response: %w", err))
	}

	// CodeBuddy returns code: 6004 or errors either on HTTP 200 or 4xx.
	if statusErr := parseError(resp.StatusCode, bodyBytes); statusErr != nil {
		provider.WarnUpstreamError(p.name, req.Model, "", statusErr.StatusCode, string(bodyBytes))
		return statusErr
	}

	if resp.StatusCode != http.StatusOK {
		provider.WarnUpstreamError(p.name, req.Model, "", resp.StatusCode, string(bodyBytes))
		return provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("upstream status %d", resp.StatusCode))
	}

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	_, err = io.Copy(w, guard.Wrap(bytes.NewReader(bodyBytes)))
	if flusher != nil {
		flusher.Flush()
	}
	return guard.Err(err)
}

// DefaultIntlModels are returned when upstream CodeBuddy Intl has no /v2/models endpoint (404) or fails.
var DefaultIntlModels = []provider.Model{
	{ID: "glm-5.2", Name: "glm-5.2"},
	{ID: "glm-5.1", Name: "glm-5.1"},
	{ID: "deepseek-v3", Name: "deepseek-v3"},
	{ID: "deepseek-v4.1-flash", Name: "deepseek-v4.1-flash"},
	{ID: "hy4-preview", Name: "hy4-preview"},
	{ID: "minimax-m3", Name: "minimax-m3"},
	{ID: "kimi-k2.7", Name: "kimi-k2.7"},
}

func (p *codebuddyProvider) Models(ctx context.Context) ([]provider.Model, error) {
	apiKey, _ := p.apiKey()
	endpoint := p.baseURL + "/models"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	p.buildHeaders(httpReq, apiKey, false)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		if p.isIntl() {
			return append([]provider.Model(nil), DefaultIntlModels...), nil
		}
		return nil, fmt.Errorf("query models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if p.isIntl() && resp.StatusCode == http.StatusNotFound {
			return append([]provider.Model(nil), DefaultIntlModels...), nil
		}
		return nil, fmt.Errorf("models query failed with status %d", resp.StatusCode)
	}

	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}

	out := make([]provider.Model, 0, len(data.Data))
	for _, m := range data.Data {
		out = append(out, provider.Model{ID: m.ID, Name: m.ID})
	}
	return out, nil
}

func (p *codebuddyProvider) Test(ctx context.Context) provider.TestResult {
	start := time.Now()
	res := provider.TestResult{}
	if _, ok := p.apiKey(); !ok {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = "no API key configured for codebuddy provider"
		return res
	}

	_, err := p.Models(ctx)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}

// parseError maps CodeBuddy error responses to typed errors. Code 6004
// (frequency limit) becomes a 429 so the combo engine drains the target.
func parseError(statusCode int, bodyText []byte) *provider.HTTPStatusError {
	if len(bodyText) == 0 {
		return nil
	}
	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(bodyText, &data); err != nil {
		return nil
	}
	if data.Code != 6004 && !rateLimitPattern.MatchString(data.Msg) {
		return nil
	}
	msg := data.Msg
	if msg == "" {
		msg = "CodeBuddy frequency limit (6004)"
	}
	return provider.NewHTTPStatusError(http.StatusTooManyRequests, msg)
}

var rateLimitPattern = regexp.MustCompile(`(?i)超出频率限制|frequency limit|限额`)
