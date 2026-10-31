package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// messagesPingInterval keeps an idle upstream from tripping Claude Code's
// stream timeout, which aborts a stream that goes silent.
const messagesPingInterval = 15 * time.Second

// handleMessages serves the Anthropic Messages API so Claude Code can use the
// gateway through ANTHROPIC_BASE_URL. The model resolves exactly as on
// /v1/chat/completions, providers keep speaking their own upstream transports,
// and only the wire the client sees is Anthropic-shaped.
func handleMessages(getCfg func() *config.Config, registry *providerRegistry, tracker *combo.Tracker, m Metrics) http.HandlerFunc {
	metrics := orNoop(m)
	return func(w http.ResponseWriter, r *http.Request) {
		startTime := time.Now()
		tc := NewTraceContext(r.Header.Get("traceparent"))
		r = r.WithContext(WithTraceContext(r.Context(), tc))
		w.Header().Set("traceparent", tc.Traceparent())

		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) {
				writeMessagesError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeMessagesError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req, err := provider.MessagesRequest(raw)
		if err != nil {
			writeMessagesError(w, http.StatusBadRequest, err.Error())
			return
		}

		cfg := getCfg()
		tx := newAnthropicTx(w, req.Model)

		if cb, ok := findCombo(cfg, req.Model); ok {
			if req.Stream {
				tx.flagStream()
			}
			responder := anthropicResponder{tx: tx}
			runCombo(responder, r, cfg, registry, cb, req, tracker, tc, startTime, metrics)
			tx.finished()
			if recorded := tx.comboError; recorded != nil && !tx.committed && recorded.body.Len() > 0 {
				writeMessagesError(w, recorded.statusCode(), comboErrorMessage(recorded.body.Bytes()))
			}
			return
		}

		p, provName, resolvedModel, ok := resolveDirect(cfg, registry, req.Model)
		if !ok {
			writeMessagesError(w, http.StatusNotFound, "model not found: "+req.Model)
			return
		}
		req.Model = resolvedModel
		if req.Stream {
			tx.flagStream()
			// Long thinking can be silent for minutes before the first delta.
			// Headers plus pings keep Claude Code from timing the stream out;
			// message_start still waits for the first real event.
			tx.begin()
		}
		attempt := tx.attempt()
		SetTelemetryHeaders(attempt, tc, provName, resolvedModel, startTime)
		providerCtx := withTokenUsage(r.Context(), metrics, provName, resolvedModel, "")
		providerErr := p.ChatCompletion(providerCtx, req, attempt)
		err = finishAttempt(attempt, providerErr)
		clientGone := r.Context().Err() != nil
		metrics.RecordProviderRequest(r.Context(), provName, resolvedModel, classifyProviderError(err, tx.committed, clientGone))
		if err != nil && !tx.committed {
			if tx.began {
				// Headers and pings are already on the wire, so a JSON body
				// would corrupt the stream and leave the ping goroutine
				// running. finished() writes the error event and stops it.
				tx.fail(errors.New(sanitizeFailure(err)))
				tx.finished()
				return
			}
			if status, msg, ok := upstreamFailure(err); ok {
				writeMessagesError(w, status, msg)
				return
			}
			writeMessagesError(w, http.StatusBadGateway, sanitizeFailure(err))
			return
		}
		if err != nil && !clientGone {
			tx.fail(errors.New(sanitizeFailure(err)))
		}
		tx.finished()
	}
}

// anthropicResponder is the Messages wire: every attempt reaches the client
// through the transaction, which translates the upstream chat frames.
type anthropicResponder struct {
	tx *anthropicTx
}

func (r anthropicResponder) attempt(stream bool) http.ResponseWriter {
	if stream {
		r.tx.flagStream()
	}
	return r.tx.attempt()
}

// writer is where runCombo writes an exhausted-combo error. It is buffered so
// the OpenAI-shaped envelope never reaches the client; the handler translates
// it through writeMessagesError.
func (r anthropicResponder) writer() http.ResponseWriter {
	if r.tx.comboError == nil {
		r.tx.comboError = &bodyRecorder{ResponseWriter: r.tx.dst}
	}
	return r.tx.comboError
}

func (r anthropicResponder) onCommittedFailure(message string) {
	r.tx.fail(errors.New(message))
	r.tx.finished()
}

// anthropicTx owns one Messages reply: the event sequence for a stream, or the
// single message object for a non-streaming call.
type anthropicTx struct {
	dst    http.ResponseWriter
	model  string
	stream bool
	id     string
	// mu serializes every write: the ping timer and the provider's parse loop
	// both write, and http.ResponseWriter is not safe for concurrent use.
	mu        sync.Mutex
	began     bool
	committed bool
	closed    bool
	// finishedOnce makes finalization idempotent: the responder finalizes a
	// committed combo failure and the handler finalizes again.
	finishedOnce bool
	// done stops the ping timer once the reply is closed.
	done chan struct{}
	// comboError captures the shared combo error envelope for translation.
	comboError *bodyRecorder
	// next is the index for the next content block opened.
	next int
	// open holds the indices of blocks that need a content_block_stop.
	open []int
	// blocks are the emitted content blocks in order.
	blocks []map[string]any
	// toolBlocks maps an upstream tool_call slot to its emitted block index.
	toolBlocks map[int]int
	partial    map[int]*strings.Builder
	text       strings.Builder
	usage      provider.TokenUsage
	// pending holds the unterminated tail of the upstream SSE line scan.
	pending []byte
	// upstreamDone reports that the upstream chunk carried a finish_reason.
	upstreamDone bool
	// stopReason is the chat finish_reason, translated on close.
	stopReason string
	// failure is the upstream error a committed stream reports.
	failure error
	// collected is the body of a non-streaming attempt.
	collected []byte
}

func newAnthropicTx(dst http.ResponseWriter, model string) *anthropicTx {
	return &anthropicTx{
		dst: dst, model: model,
		id:         "msg_" + randomHex(12),
		toolBlocks: map[int]int{},
		partial:    map[int]*strings.Builder{},
		done:       make(chan struct{}),
	}
}

func (t *anthropicTx) attempt() http.ResponseWriter {
	return &anthropicAttempt{tx: t}
}

func (t *anthropicTx) Header() http.Header { return t.dst.Header() }

func (t *anthropicTx) WriteHeader(int) {}

func (t *anthropicTx) Write(data []byte) (int, error) { return t.dst.Write(data) }

func (t *anthropicTx) flagStream() { t.stream = true }

func (t *anthropicTx) fail(err error) { t.failure = err }

// emit writes one SSE event. nil payload writes the event name alone; a nil
// data field is kept out of the JSON, matching the API's ping event.
func (t *anthropicTx) emit(name string, data map[string]any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return
	}
	buf := make([]byte, 0, len(encoded)+len(name)+16)
	buf = append(buf, "event: "...)
	buf = append(buf, name...)
	buf = append(buf, "\ndata: "...)
	buf = append(buf, encoded...)
	buf = append(buf, "\n\n"...)
	_, _ = t.dst.Write(buf)
	if f, ok := t.dst.(http.Flusher); ok {
		f.Flush()
	}
}

// ping runs until the reply closes, keeping the stream alive while the
// upstream is silent (long thinking turns emit no chunks).
func (t *anthropicTx) ping() {
	ticker := time.NewTicker(messagesPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			t.emit("ping", map[string]any{"type": "ping"})
		}
	}
}

// message renders the accumulated reply as an Anthropic message object.
func (t *anthropicTx) message(stopReason string, content []any) map[string]any {
	return map[string]any{
		"id": t.id, "type": "message", "role": "assistant", "model": t.model,
		"content": content, "stop_reason": stopReason, "stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  t.usage.InputTokens,
			"output_tokens": t.usage.OutputTokens,
		},
	}
}

// finished closes a reply: the full event sequence for a stream, the single
// object for a collected body.
func (t *anthropicTx) finished() {
	if !t.stream {
		t.renderObject()
		return
	}
	t.mu.Lock()
	if t.finishedOnce {
		t.mu.Unlock()
		return
	}
	// Claim the finish before releasing the lock: a committed combo failure
	// finalizes from onCommittedFailure and again from the handler, and only
	// one may stop the ping goroutine and write the terminal events.
	t.finishedOnce = true
	if !t.committed && !t.began {
		// Nothing was emitted, so the caller answers with a normal error body.
		t.closed = true
		t.mu.Unlock()
		close(t.done)
		return
	}
	began := t.began
	t.mu.Unlock()
	close(t.done)

	if began && !t.committed {
		// Keepalives already went out, so a JSON body would corrupt the
		// stream; report the failure as the terminal event instead.
		message := "upstream returned no reply"
		if t.failure != nil {
			message = t.failure.Error()
		}
		t.emit("error", map[string]any{
			"type": "error", "error": map[string]any{"type": "api_error", "message": message},
		})
		return
	}
	if t.failure != nil {
		t.emit("error", map[string]any{
			"type": "error", "error": map[string]any{"type": "api_error", "message": t.failure.Error()},
		})
		return
	}
	t.closeBlocks()
	t.fillToolInputs()
	stop := anthropicStopReason(t.stopReason)
	t.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": t.usage.OutputTokens},
	})
	t.emit("message_stop", map[string]any{"type": "message_stop"})
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}

// begin sends the SSE headers and starts keepalives before the provider runs,
// so a silent upstream does not time the client out. It does not commit: a
// failure here still belongs to failover, but the response can no longer carry
// a JSON error body, so a pre-commit failure ends the stream with an error
// event instead.
func (t *anthropicTx) begin() {
	t.mu.Lock()
	if t.began {
		t.mu.Unlock()
		return
	}
	t.began = true
	t.dst.Header().Set("Content-Type", "text/event-stream")
	t.dst.Header().Set("Cache-Control", "no-cache")
	t.dst.WriteHeader(http.StatusOK)
	t.mu.Unlock()
	go t.ping()
}

// start emits message_start and opens the reply. Headers are written under the
// mutex so the ping goroutine cannot interleave with them.
// openTextBlock follows with the first content block.
func (t *anthropicTx) start() {
	t.begin() // no-op when the handler already sent headers
	t.mu.Lock()
	t.committed = true
	t.mu.Unlock()
	t.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": t.id, "type": "message", "role": "assistant", "model": t.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens": t.usage.InputTokens, "output_tokens": 0,
				"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
			},
		},
	})
}

func (t *anthropicTx) openTextBlock() int {
	index := t.next
	t.next++
	t.blocks = append(t.blocks, map[string]any{"type": "text", "text": ""})
	t.open = append(t.open, index)
	t.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": index,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	return index
}

func (t *anthropicTx) closeBlocks() {
	for _, index := range t.open {
		t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	}
	t.open = nil
}

// closeLast stops the innermost open block, so a text block ends before a
// tool block begins and the client's block state stays sequential.
func (t *anthropicTx) closeLast() {
	if len(t.open) == 0 {
		return
	}
	index := t.open[len(t.open)-1]
	t.open = t.open[:len(t.open)-1]
	t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

// parse consumes the Chat Completions SSE bytes the provider writes and emits
// the Anthropic events they imply.
func (t *anthropicTx) parse(data []byte) {
	t.pending = append(t.pending, data...)
	for {
		index := bytes.IndexByte(t.pending, '\n')
		if index < 0 {
			return
		}
		t.line(bytes.TrimRight(t.pending[:index], "\r"))
		t.pending = t.pending[index+1:]
	}
}

func (t *anthropicTx) line(line []byte) {
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}
	if usage, found := provider.ChatUsageFromSSELine(line); found {
		t.usage = usage
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != "" {
			t.delta(choice.Delta.Content)
		}
		for _, call := range choice.Delta.ToolCalls {
			t.toolDelta(call.Index, call.ID, call.Function.Name, call.Function.Arguments)
		}
		if choice.FinishReason != nil {
			t.upstreamDone = true
			t.stopReason = *choice.FinishReason
		}
	}
}

func (t *anthropicTx) delta(text string) {
	if !t.committed {
		t.start()
	}
	if len(t.open) == 0 || t.blocks[t.open[len(t.open)-1]]["type"] != "text" {
		if len(t.open) > 0 {
			t.closeLast()
		}
		t.openTextBlock()
	}
	index := t.open[len(t.open)-1]
	block := t.blocks[index]
	block["text"] = block["text"].(string) + text
	t.text.WriteString(text)
	t.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

// toolDelta opens a tool_use block on the first fragment and streams the
// arguments as input_json_delta, which is how Claude Code reassembles input.
func (t *anthropicTx) toolDelta(index *int, id, name, arguments string) {
	if !t.committed {
		t.start()
	}
	slot := 0
	if index != nil {
		slot = *index
	}
	blockIndex, ok := t.toolBlocks[slot]
	if !ok {
		if len(t.open) > 0 && t.blocks[t.open[len(t.open)-1]]["type"] == "text" {
			t.closeLast()
		}
		blockIndex = t.next
		t.next++
		t.toolBlocks[slot] = blockIndex
		t.blocks = append(t.blocks, map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": map[string]any{},
		})
		t.partial[blockIndex] = &strings.Builder{}
		t.open = append(t.open, blockIndex)
		t.emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": blockIndex,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
		})
	}
	if arguments == "" {
		return
	}
	t.partial[blockIndex].WriteString(arguments)
	t.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": blockIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments},
	})
}

// fillToolInputs replaces each streamed tool block's placeholder with the
// arguments reassembled from its input_json_delta events.
func (t *anthropicTx) fillToolInputs() {
	for index, partial := range t.partial {
		if partial.Len() == 0 {
			continue
		}
		var input any
		if err := json.Unmarshal([]byte(partial.String()), &input); err != nil {
			input = map[string]any{}
		}
		t.blocks[index]["input"] = input
	}
}

// renderObject writes the non-streaming message from the collected chat body.
func (t *anthropicTx) renderObject() {
	if t.failure != nil || !t.committed {
		return
	}
	var chat struct {
		Choices []struct {
			Message struct {
				Content   any `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(t.collected, &chat); err != nil {
		writeMessagesError(t.dst, http.StatusBadGateway, "upstream returned malformed JSON")
		return
	}
	var content []any
	stopReason := "end_turn"
	if len(chat.Choices) > 0 {
		choice := chat.Choices[0]
		if text := chatText(choice.Message.Content); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, call := range choice.Message.ToolCalls {
			var input any = map[string]any{}
			if call.Function.Arguments != "" {
				var decoded any
				if err := json.Unmarshal([]byte(call.Function.Arguments), &decoded); err == nil {
					input = decoded
				}
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input,
			})
		}
		stopReason = anthropicStopReason(choice.FinishReason)
	}
	if chat.Usage != nil {
		t.usage = provider.TokenUsage{Present: true, InputTokens: chat.Usage.PromptTokens, OutputTokens: chat.Usage.CompletionTokens}
	}
	encoded, err := json.Marshal(t.message(stopReason, content))
	if err != nil {
		writeMessagesError(t.dst, http.StatusBadGateway, "upstream returned malformed JSON")
		return
	}
	t.dst.Header().Set("Content-Type", "application/json")
	t.dst.WriteHeader(http.StatusOK)
	_, _ = t.dst.Write(encoded)
}

func anthropicStopReason(reason string) string {
	switch reason {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	}
	// A chat "stop" may or may not have been a custom stop sequence, and the
	// upstream does not say; end_turn is the safe reading.
	return "end_turn"
}

// anthropicAttempt is what a provider writes into. It buffers a non-streaming
// body until the attempt is proven good, and re-frames a stream as it arrives.
type anthropicAttempt struct {
	tx   *anthropicTx
	body bytes.Buffer
}

func (a *anthropicAttempt) Header() http.Header { return a.tx.Header() }

func (a *anthropicAttempt) WriteHeader(int) {}

func (a *anthropicAttempt) Write(data []byte) (int, error) {
	if a.tx.stream {
		a.tx.parse(data)
		return len(data), nil
	}
	return a.body.Write(data)
}

func (a *anthropicAttempt) Flush() {}

// finish keeps the combo loop's failover rules: a streaming attempt commits
// with its first event, a collected one once it holds a reply.
func (a *anthropicAttempt) finish(err error) error {
	if a.tx.stream {
		if err != nil {
			if a.tx.committed {
				return errors.Join(errResponseCommitted, err)
			}
			return err
		}
		if !a.tx.upstreamDone {
			// Once events are on the wire no other target can take over: a
			// plain error would be retried by the combo loop into a message
			// that already started. Committed truncation is terminal.
			if a.tx.committed {
				return errors.Join(errResponseCommitted, errors.New("upstream returned no reply"))
			}
			return errors.New("upstream returned no reply")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if a.body.Len() == 0 {
		return errors.New("upstream returned an empty response")
	}
	if !json.Valid(a.body.Bytes()) {
		return errors.New("upstream returned malformed JSON")
	}
	if chatJSONWithoutReply(a.body.Bytes()) {
		return errors.New("upstream returned no reply")
	}
	a.tx.collected = append([]byte(nil), a.body.Bytes()...)
	a.tx.committed = true
	return nil
}

// writeMessagesError writes the Anthropic error envelope. Claude Code matches
// on the message text to decide automatic retries, so upstream messages are
// passed through rather than replaced.
func writeMessagesError(w http.ResponseWriter, status int, msg string) {
	kind := "invalid_request_error"
	switch {
	case status == http.StatusUnauthorized:
		kind = "authentication_error"
	case status == http.StatusNotFound:
		kind = "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		kind = "request_too_large"
	case status == http.StatusTooManyRequests:
		kind = "rate_limit_error"
	case status >= http.StatusInternalServerError:
		kind = "api_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": kind, "message": msg},
	})
}

// comboErrorMessage pulls the message out of the shared OpenAI error envelope
// runCombo wrote, so the Anthropic envelope keeps the text the client retries
// on.
func comboErrorMessage(body []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return "upstream failure"
}

// upstreamFailure extracts the status and sanitized message from an upstream
// HTTP error, so the Anthropic envelope carries what the client retries on.
func upstreamFailure(err error) (int, string, bool) {
	var status interface{ HTTPStatus() int }
	if !errors.As(err, &status) {
		return 0, "", false
	}
	code := status.HTTPStatus()
	if code < 400 || code > 599 {
		return 0, "", false
	}
	return code, sanitizeFailure(err), true
}
