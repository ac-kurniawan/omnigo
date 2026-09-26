package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sseProxyState carries the reusable buffers for one proxied SSE stream: the
// read buffer and the accumulator for the line currently being scanned.
type sseProxyState struct {
	read []byte
	line []byte
	// lost is set when the current line outgrew maxSSELine and stopped being
	// inspected; it clears at the next line boundary.
	lost bool
	// usage is the last Chat Completions usage object seen. A later object
	// replaces it; the proxy reports it only after the stream completes.
	usage TokenUsage
}

var sseProxyPool = sync.Pool{
	New: func() any {
		return &sseProxyState{
			read: make([]byte, 64*1024),
			line: make([]byte, 0, 4096),
		}
	},
}

// maxSSELine bounds the inspected part of one SSE line so a malformed upstream
// cannot grow the scan without limit. A longer line stops being inspected; its
// bytes still reach the client untouched.
const maxSSELine = 4 * 1024 * 1024

// maxRetainedLine caps the accumulator capacity a pooled state keeps, so one
// pathological line cannot pin memory in the pool forever.
const maxRetainedLine = 64 * 1024

// errIncompleteStream reports a streaming upstream that ended without a
// completion signal: no finish_reason on any chunk and no [DONE] sentinel.
// Returning it lets a combo fail the attempt over instead of handing the
// client a truncated body that looks like a finished answer.
var errIncompleteStream = errors.New("openai: incomplete SSE response")

var (
	sseDataPrefix = []byte("data:")
	sseDone       = []byte("[DONE]")
)

// finishReasonKey is the exact bytes of the finish_reason field name, so the
// completeness scan can skip the JSON decode of every content chunk.
var finishReasonKey = []byte(`"finish_reason"`)

// sseCompletionSignal is the chunk shape the completeness scan needs. Only
// choices are decoded; everything else (usage, id, model) is ignored, so a
// provider's extra fields never affect the check.
type sseCompletionSignal struct {
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

type openAIProvider struct {
	name          string
	baseURL       string
	store         CredStore
	client        *http.Client
	stream        *http.Client
	idle          time.Duration
	streamTimeout time.Duration
}

func NewOpenAI(cfg Config, store CredStore) Provider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	client := &http.Client{Timeout: timeout, Transport: cfg.Transport}
	return &openAIProvider{
		name:          cfg.Name,
		baseURL:       strings.TrimRight(cfg.BaseURL, "/"),
		store:         store,
		client:        client,
		stream:        StreamClient(client),
		idle:          timeout,
		streamTimeout: cfg.StreamTimeout,
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
	guard := NewIdleGuard(p.idle, StreamBudget(p.streamTimeout, req.Stream), func() { cancel(ErrUpstreamStall) })
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
	if req.Stream {
		return proxyStream(ctx, guard.Wrap(resp.Body), w, flusher)
	}
	err = copyJSONUsage(ctx, guard.Wrap(resp.Body), w)
	return err
}

// proxyStream forwards an upstream SSE body to the client byte for byte while
// watching for a completion signal and a Chat Completions usage object.
// Framing is never rewritten: whatever line terminators the upstream uses
// reach the client unchanged. A stream that ends without a finish_reason
// chunk or a [DONE] frame is reported as a failure, so a truncated answer is
// not mistaken for a finished one. Usage seen on that truncated stream is
// dropped; a completed stream reports the last usage object, once.
func proxyStream(ctx context.Context, reader io.Reader, w http.ResponseWriter, flusher http.Flusher) error {
	state := sseProxyPool.Get().(*sseProxyState)
	defer func() {
		state.release()
		sseProxyPool.Put(state)
	}()
	finished := false
	for {
		n, rErr := reader.Read(state.read)
		if n > 0 {
			finished = state.scanLines(state.read[:n]) || finished
			if _, wErr := w.Write(state.read[:n]); wErr != nil {
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
	if !finished {
		// An unterminated final line still counts: some providers close the
		// stream right after the last frame without a trailing newline.
		finished = state.pendingComplete()
	}
	if !finished {
		return errIncompleteStream
	}
	// The trailing line can be a usage object with no newline. A completion
	// signal is not one, so this only fills usage the line scan has not seen.
	if !state.lost {
		state.observeUsage(state.line)
	}
	ReportUsage(ctx, state.usage)
	return nil
}

// scanLines consumes the complete lines in chunk, records the last Chat
// Completions usage object, and reports whether any line proves the stream
// finished. An unterminated tail is accumulated for the next chunk, and the
// accumulator holds the line without its terminator. Scanning continues after
// the first completion signal: the usage chunk is a later frame.
func (s *sseProxyState) scanLines(chunk []byte) bool {
	finished := false
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			s.addLine(chunk)
			return finished
		}
		s.addLine(chunk[:i])
		if !s.lost {
			if isCompletionSignal(s.line) {
				finished = true
			}
			s.observeUsage(s.line)
		}
		s.line, s.lost = s.line[:0], false
		chunk = chunk[i+1:]
	}
	return finished
}

// addLine accumulates a partial line, giving up on one that exceeds maxSSELine.
func (s *sseProxyState) addLine(p []byte) {
	if s.lost {
		return
	}
	if len(s.line)+len(p) > maxSSELine {
		s.lost = true
		s.line = s.line[:0]
		return
	}
	s.line = append(s.line, p...)
}

// pendingComplete reports whether the trailing, unterminated line is itself a
// completion signal.
func (s *sseProxyState) pendingComplete() bool {
	return !s.lost && isCompletionSignal(s.line)
}

// observeUsage keeps the last Chat Completions usage object on a data line.
// A line that is not a usage chunk leaves the previous object in place, so a
// trailing [DONE] frame does not erase the count.
func (s *sseProxyState) observeUsage(line []byte) {
	if usage, ok := chatUsageFromSSELine(line); ok {
		s.usage = usage
	}
}

// release clears the per-stream scan state before the state returns to the
// pool, dropping an accumulator that grew for a pathological line and the
// usage object so the next stream cannot inherit it.
func (s *sseProxyState) release() {
	s.lost = false
	s.usage = TokenUsage{}
	if cap(s.line) > maxRetainedLine {
		s.line = make([]byte, 0, 4096)
		return
	}
	s.line = s.line[:0]
}

// isCompletionSignal reports whether one SSE line ends the stream: either a
// [DONE] sentinel or a chunk whose choices carry a non-null finish_reason.
// Only data lines count; comments and other fields never complete a stream.
// The JSON decode runs only on lines that contain the field, so content chunks
// (which carry a null finish_reason) are not decoded.
func isCompletionSignal(line []byte) bool {
	payload, ok := bytes.CutPrefix(line, sseDataPrefix)
	if !ok {
		return false
	}
	payload = bytes.TrimSpace(payload)
	if bytes.Equal(payload, sseDone) {
		return true
	}
	if !bytes.Contains(payload, finishReasonKey) {
		return false
	}
	var chunk sseCompletionSignal
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return false
	}
	for _, c := range chunk.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			return true
		}
	}
	return false
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

// chatUsageWire is the Chat Completions usage object. Responses API names
// (input_tokens, output_tokens) are a different shape and are not read.
// cache_creation is not a field of this API, so it stays zero.
type chatUsageWire struct {
	PromptTokens     flexInt `json:"prompt_tokens"`
	CompletionTokens flexInt `json:"completion_tokens"`
	PromptDetails    *struct {
		CachedTokens flexInt `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens flexInt `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// flexInt accepts a JSON integer or an integer-valued number. Compatible
// gateways sometimes emit 11.0. A fractional or negative count rejects the
// whole usage object so it is not reported as a partial sample.
type flexInt struct {
	set bool
	n   int
}

func (f *flexInt) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	n, ok := integerTokenCount(b)
	if !ok {
		return fmt.Errorf("token count must be a non-negative integer")
	}
	f.set, f.n = true, n
	return nil
}

func integerTokenCount(b []byte) (int, bool) {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		return n, n >= 0
	}
	var x float64
	if err := json.Unmarshal(b, &x); err != nil || x < 0 || x != math.Trunc(x) || x > math.MaxInt {
		return 0, false
	}
	return int(x), true
}

func (w chatUsageWire) present() bool {
	if w.PromptTokens.set || w.CompletionTokens.set {
		return true
	}
	if w.PromptDetails != nil && w.PromptDetails.CachedTokens.set {
		return true
	}
	return w.CompletionDetails != nil && w.CompletionDetails.ReasoningTokens.set
}

func (w chatUsageWire) tokenUsage() TokenUsage {
	usage := TokenUsage{Present: true}
	if w.PromptTokens.set {
		usage.InputTokens = w.PromptTokens.n
	}
	if w.CompletionTokens.set {
		usage.OutputTokens = w.CompletionTokens.n
	}
	if w.PromptDetails != nil && w.PromptDetails.CachedTokens.set {
		usage.CachedTokens = w.PromptDetails.CachedTokens.n
	}
	if w.CompletionDetails != nil && w.CompletionDetails.ReasoningTokens.set {
		usage.ReasoningTokens = w.CompletionDetails.ReasoningTokens.n
	}
	return usage
}

// chatUsageFromSSELine reads usage from one SSE data line. Non-data lines,
// the [DONE] sentinel, and chunks with no usage object report ok false.
func chatUsageFromSSELine(line []byte) (TokenUsage, bool) {
	payload, ok := bytes.CutPrefix(line, sseDataPrefix)
	if !ok {
		return TokenUsage{}, false
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' || !bytes.Contains(payload, usageKey) {
		return TokenUsage{}, false
	}
	var chunk struct {
		Usage *chatUsageWire `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil || chunk.Usage == nil || !chunk.Usage.present() {
		return TokenUsage{}, false
	}
	return chunk.Usage.tokenUsage(), true
}

// usageKey is the exact bytes of the usage field name, so content chunks skip
// the JSON decode the same way the finish_reason scan does.
var usageKey = []byte(`"usage"`)

// maxUsageScan bounds how much of a non-streaming body is retained to find a
// usage object. The client still receives every byte. A response that does not
// fit is delivered intact and left unaccounted: accounting must not pin an
// unbounded completion, and must not fail the proxy when it cannot measure.
const maxUsageScan = 1 << 20

// copyJSONUsage copies a non-streaming body to the client unchanged and, when
// the whole body fits in maxUsageScan, reports the Chat Completions usage
// object. A missing or unreadable usage object reports nothing. The copy is
// success even when accounting is skipped.
func copyJSONUsage(ctx context.Context, reader io.Reader, w io.Writer) error {
	var scanned bytes.Buffer
	truncated := false
	buf := make([]byte, 32*1024)
	for {
		n, rErr := reader.Read(buf)
		if n > 0 {
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				return wErr
			}
			if truncated || scanned.Len() >= maxUsageScan {
				truncated = true
			} else if remain := maxUsageScan - scanned.Len(); n > remain {
				truncated = true
				scanned.Write(buf[:remain])
			} else {
				scanned.Write(buf[:n])
			}
		}
		if rErr != nil {
			if rErr == io.EOF {
				break
			}
			return rErr
		}
	}
	if truncated {
		return nil
	}
	if usage, ok := chatUsageFromJSON(scanned.Bytes()); ok {
		ReportUsage(ctx, usage)
	}
	return nil
}

// chatUsageFromJSON reads the top-level usage object. usage null, a non-object
// usage, and a body that does not fit the scan are absent, not zero tokens.
func chatUsageFromJSON(body []byte) (TokenUsage, bool) {
	var parsed struct {
		Usage *chatUsageWire `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Usage == nil || !parsed.Usage.present() {
		return TokenUsage{}, false
	}
	return parsed.Usage.tokenUsage(), true
}
