package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		return proxyStream(guard.Wrap(resp.Body), w, flusher)
	}
	_, err = io.Copy(w, guard.Wrap(resp.Body))
	return err
}

// proxyStream forwards an upstream SSE body to the client byte for byte while
// watching for a completion signal. Framing is never rewritten: whatever line
// terminators the upstream uses reach the client unchanged. A stream that ends
// without a finish_reason chunk or a [DONE] frame is reported as a failure, so
// a truncated answer is not mistaken for a finished one.
func proxyStream(reader io.Reader, w http.ResponseWriter, flusher http.Flusher) error {
	state := sseProxyPool.Get().(*sseProxyState)
	defer func() {
		state.release()
		sseProxyPool.Put(state)
	}()
	finished := false
	for {
		n, rErr := reader.Read(state.read)
		if n > 0 {
			if !finished {
				finished = state.scanLines(state.read[:n])
			}
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
	return nil
}

// scanLines consumes the complete lines in chunk and reports whether any of
// them proves the stream finished. An unterminated tail is accumulated for the
// next chunk, and the accumulator holds the line without its terminator.
func (s *sseProxyState) scanLines(chunk []byte) bool {
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			s.addLine(chunk)
			return false
		}
		s.addLine(chunk[:i])
		complete := !s.lost && isCompletionSignal(s.line)
		s.line, s.lost = s.line[:0], false
		if complete {
			return true
		}
		chunk = chunk[i+1:]
	}
	return false
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

// release clears the per-stream scan state before the state returns to the
// pool, dropping an accumulator that grew for a pathological line.
func (s *sseProxyState) release() {
	s.lost = false
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
