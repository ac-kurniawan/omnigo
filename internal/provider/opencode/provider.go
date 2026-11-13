package opencode

import (
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
	"strings"
	"sync"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/provider/codex"
)

const (
	defaultBaseURL   = "https://opencode.ai/zen/go/v1"
	customUserAgent  = "omnigo/1.0"
	sessionHeader    = "x-opencode-session"
	anthropicVersion = "2023-06-01"
)

func init() {
	provider.Register("opencode", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return New(cfg, store)
	})
}

type Provider struct {
	name          string
	baseURL       string
	store         provider.CredStore
	client        *http.Client
	stream        *http.Client
	idle          time.Duration
	streamTimeout time.Duration
	catalogMu     sync.RWMutex
	catalog       []provider.Model
	protoRoutes   map[string]protocol
}

func New(cfg provider.Config, store provider.CredStore) provider.Provider {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	client := &http.Client{Timeout: timeout, Transport: cfg.Transport}
	return &Provider{
		name:          cfg.Name,
		baseURL:       baseURL,
		store:         store,
		client:        client,
		stream:        provider.StreamClient(client),
		idle:          timeout,
		streamTimeout: cfg.StreamTimeout,
	}
}

func (p *Provider) Name() string {
	return p.name
}

// resolveProtocol prefers the live models-endpoint contract cached by Models();
// unknown models fall back to the prefix classifier so the hot path never
// blocks on network.
func (p *Provider) resolveProtocol(model string) protocol {
	p.catalogMu.RLock()
	proto, ok := p.protoRoutes[model]
	p.catalogMu.RUnlock()
	if ok {
		return proto
	}
	return classifyModel(model)
}

func (p *Provider) ChatCompletion(ctx context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
	creds := p.store.Get()
	token := creds.APIKey
	if token == "" {
		token = creds.AccessToken
	}
	if token == "" {
		return provider.NewHTTPStatusError(http.StatusUnauthorized, "opencode: missing api key")
	}

	proto := p.resolveProtocol(req.Model)
	switch proto {
	case protocolMessages:
		return p.chatMessages(ctx, req, token, creds, w)
	case protocolResponses:
		return p.chatResponses(ctx, req, token, creds, w)
	default:
		return p.chatCompletions(ctx, req, token, creds, w)
	}
}

// stableSessionID returns a ses_<32-hex> string.
// If an incoming session id is present in the request body/context, it derives a stable
// 32-hex representation; otherwise, it generates a cryptographically random 32-hex ID.
func stableSessionID(ctx context.Context, req provider.ChatRequest) string {
	var key string
	payload := req.Parsed
	if payload != nil {
		for _, f := range []string{"session_id", "prompt_cache_key", "conversation_id"} {
			if v, ok := payload[f].(string); ok && v != "" {
				key = v
				break
			}
		}
	}
	if key == "" {
		if caller := auth.CallerFrom(ctx); caller != nil && caller.ID() != "" {
			key = caller.ID()
		}
	}
	if key != "" {
		// If key already matches ses_<32 hex chars>, preserve it directly.
		if strings.HasPrefix(key, "ses_") && len(key) == 36 {
			isHex := true
			for _, r := range key[4:] {
				if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
					isHex = false
					break
				}
			}
			if isHex {
				return strings.ToLower(key)
			}
		}
		sum := sha256.Sum256([]byte(key))
		return "ses_" + hex.EncodeToString(sum[:16])
	}

	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "ses_00000000000000000000000000000000"
	}
	return "ses_" + hex.EncodeToString(buf[:])
}

func (p *Provider) setCommonHeaders(httpReq *http.Request, sessionID string) {
	httpReq.Header.Set("User-Agent", customUserAgent)
	if sessionID != "" {
		httpReq.Header.Set(sessionHeader, sessionID)
	}
}

func (p *Provider) chatCompletions(ctx context.Context, req provider.ChatRequest, token string, creds provider.Credentials, w http.ResponseWriter) error {
	sessionID := stableSessionID(ctx, req)
	body, err := req.Body()
	if err != nil {
		return err
	}

	endpoint := endpointFor(p.baseURL, protocolChat)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	p.setCommonHeaders(httpReq, sessionID)

	client := p.client
	if req.Stream {
		client = p.stream
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("opencode: upstream status %d", resp.StatusCode))
	}

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if req.Stream {
		return proxyStream(ctx, resp.Body, creds.Identity(), w)
	}
	return copyJSONBodyWithUsage(ctx, resp.Body, creds.Identity(), w)
}

func (p *Provider) chatMessages(ctx context.Context, req provider.ChatRequest, token string, creds provider.Credentials, w http.ResponseWriter) error {
	sessionID := stableSessionID(ctx, req)
	msgBody, err := toAnthropicMessagesBody(req)
	if err != nil {
		return err
	}

	endpoint := endpointFor(p.baseURL, protocolMessages)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(msgBody))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", token)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	p.setCommonHeaders(httpReq, sessionID)

	client := p.client
	if req.Stream {
		client = p.stream
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("opencode: upstream status %d", resp.StatusCode))
	}

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		return streamAnthropicToChat(ctx, resp.Body, req.Model, creds.Identity(), w)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return completeAnthropicToChat(ctx, resp.Body, req.Model, creds.Identity(), w)
}

func (p *Provider) chatResponses(ctx context.Context, req provider.ChatRequest, token string, creds provider.Credentials, w http.ResponseWriter) error {
	sessionID := stableSessionID(ctx, req)
	responsesPayload, err := codex.ToResponsesRequest(req)
	if err != nil {
		return err
	}

	// Always require streaming upstream for Responses protocol
	responsesPayload["stream"] = true

	reqBody, err := json.Marshal(responsesPayload)
	if err != nil {
		return err
	}

	endpoint := endpointFor(p.baseURL, protocolResponses)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	p.setCommonHeaders(httpReq, sessionID)

	resp, err := p.stream.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("opencode: upstream status %d", resp.StatusCode))
	}

	if req.Stream {
		return streamResponsesToChat(ctx, resp.Body, req.Model, creds, w)
	}
	return completeResponsesToChat(ctx, resp.Body, req.Model, creds, w)
}

// allowedResponseHeaders lists the upstream headers worth forwarding to a
// gateway client. Everything else stays upstream: Set-Cookie and
// WWW-Authenticate describe a session the client does not own, and internal
// routing/debug headers disclose infrastructure detail. Mirrors the allowlist
// in internal/provider/openai.go.
var allowedResponseHeaders = map[string]bool{
	"Content-Type":     true,
	"Cache-Control":    true,
	"Content-Encoding": true,
	"Retry-After":      true,
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if !allowedResponseHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func copyJSONBodyWithUsage(ctx context.Context, r io.Reader, account string, w io.Writer) error {
	body, err := provider.ReadCompletionBody(r)
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	// The body is one JSON object, not a framed stream. ChatUsageFromSSELine
	// expects a data line, so pass the offset it looks past instead of copying
	// the whole body to prepend a prefix.
	if usage, found := provider.ChatUsageFromJSONLine(body); found {
		usage.Account = account
		provider.ReportUsage(ctx, usage)
	}
	return nil
}

// proxyStream forwards an upstream stream verbatim while counting the usage
// object on its data lines.
func proxyStream(ctx context.Context, r io.Reader, account string, w http.ResponseWriter) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	scanner := &lineScanner{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				return wErr
			}
			if flusher != nil {
				flusher.Flush()
			}
			scanner.add(buf[:n], func(line []byte) {
				if !bytes.HasPrefix(line, []byte("data:")) {
					return
				}
				if usage, found := provider.ChatUsageFromSSELine(line); found {
					usage.Account = account
					provider.ReportUsage(ctx, usage)
				}
			})
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// lineScanner splits a byte stream into lines while holding at most one
// unterminated tail. A line longer than provider.MaxSSELine is never returned
// and its remaining bytes are dropped as they arrive: the accumulator is
// discarded instead of grown, so a hostile upstream cannot make this process
// hold an arbitrarily long line. Scanning resumes at the next newline.
type lineScanner struct {
	// limit caps one line; zero means provider.MaxSSELine. Tests set it small.
	limit int
	// pending holds the current line; start skips the lines already returned.
	// The two are compacted once per add call, so a stream costs one copy per
	// read rather than one per line.
	pending []byte
	start   int
	// discarding marks the tail of an overlong line, whose bytes are dropped
	// until a newline arrives.
	discarding bool
}

// add appends one chunk and hands every complete, in-bounds line to emit. The
// slice passed to emit aliases the scanner's buffer, so it must be consumed
// before add returns; nothing is retained, which is why the buffer can be
// compacted in place.
func (s *lineScanner) add(chunk []byte, emit func(line []byte)) {
	if s.discarding {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			return
		}
		s.discarding = false
		chunk = chunk[i+1:]
	}
	s.pending = append(s.pending, chunk...)
	for s.start < len(s.pending) {
		i := bytes.IndexByte(s.pending[s.start:], '\n')
		if i < 0 {
			break
		}
		emit(s.pending[s.start : s.start+i])
		s.start += i + 1
	}
	limit := s.limit
	if limit <= 0 {
		limit = provider.MaxSSELine
	}
	if len(s.pending)-s.start > limit {
		// Drop the oversized accumulator, and the capacity it grew to, so the
		// next line starts small instead of reusing a cap-sized buffer.
		s.discarding = true
		s.pending, s.start = nil, 0
		return
	}
	if s.start > 0 {
		s.pending = s.pending[:copy(s.pending, s.pending[s.start:])]
		s.start = 0
	}
}
