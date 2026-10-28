package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// handleResponses serves the stateless subset of the OpenAI Responses API. The
// model resolves exactly as on /v1/chat/completions, providers keep speaking
// their own upstream transports, and only the wire the client sees is
// Responses-shaped.
func handleResponses(getCfg func() *config.Config, registry *providerRegistry, tracker *combo.Tracker, m Metrics) http.HandlerFunc {
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
				writeResponsesError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeResponsesError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req, err := provider.ResponsesRequest(raw)
		if err != nil {
			writeResponsesError(w, http.StatusBadRequest, err.Error())
			return
		}

		cfg := getCfg()
		tx := newResponsesTx(w, req.Model)

		if cb, ok := findCombo(cfg, req.Model); ok {
			runCombo(responsesResponder{tx: tx}, r, cfg, registry, cb, req, tracker, tc, startTime, metrics)
			tx.finished()
			return
		}

		p, provName, resolvedModel, ok := resolveDirect(cfg, registry, req.Model)
		if !ok {
			writeResponsesError(w, http.StatusNotFound, "model not found: "+req.Model)
			return
		}
		req.Model = resolvedModel
		if req.Stream {
			tx.flagStream()
		}
		attempt := tx.attempt()
		SetTelemetryHeaders(attempt, tc, provName, resolvedModel, startTime)
		providerCtx := withTokenUsage(r.Context(), metrics, provName, resolvedModel, "")
		providerErr := p.ChatCompletion(providerCtx, req, attempt)
		err = finishAttempt(attempt, providerErr)
		clientGone := r.Context().Err() != nil
		metrics.RecordProviderRequest(r.Context(), provName, resolvedModel, classifyProviderError(err, tx.committed, clientGone))
		if err != nil && !tx.committed {
			writeProviderError(w, err)
			return
		}
		if err != nil && !clientGone {
			tx.fail(errors.New(sanitizeFailure(err)))
		}
		tx.finished()
	}
}

// responsesResponder is the Responses wire: every attempt reaches the client
// through the transaction, which translates the upstream chat frames.
type responsesResponder struct {
	tx *responsesTx
}

func (r responsesResponder) attempt(stream bool) http.ResponseWriter {
	if stream {
		r.tx.flagStream()
	}
	return r.tx.attempt()
}

func (r responsesResponder) writer() http.ResponseWriter { return r.tx.dst }

func (r responsesResponder) onCommittedFailure(message string) {
	r.tx.fail(errors.New(message))
	r.tx.finished()
}

// responsesTx owns one Responses reply: the synthesized event sequence for a
// stream, or the single object for a non-streaming call. The provider writes
// Chat Completions frames into an attempt writer and never sees Responses
// shapes.
type responsesTx struct {
	dst       http.ResponseWriter
	model     string
	stream    bool
	id        string
	created   int64
	seq       int
	committed bool
	// upstreamDone reports that the upstream chunk carried a finish_reason.
	upstreamDone bool
	// closed reports that the terminal events have been written.
	closed bool
	text   strings.Builder
	usage  provider.TokenUsage
	// pending holds the unterminated tail of the upstream SSE line scan.
	pending []byte
	// failure is the upstream error a committed stream reports.
	failure error
	// collected is the body of a non-streaming attempt.
	collected []byte
}

func newResponsesTx(dst http.ResponseWriter, model string) *responsesTx {
	return &responsesTx{dst: dst, model: model, id: "resp_" + randomHex(12), created: time.Now().Unix(), seq: 1}
}

// attempt returns the writer the provider writes into. A streaming attempt is
// re-framed live; a non-streaming one is collected and rendered on finish.
func (t *responsesTx) attempt() http.ResponseWriter {
	return &responsesAttempt{tx: t}
}

// Header exposes the destination headers, which both attempt writers set.
func (t *responsesTx) Header() http.Header { return t.dst.Header() }

func (t *responsesTx) WriteHeader(int) {}

func (t *responsesTx) Write(data []byte) (int, error) {
	return t.dst.Write(data)
}

func (t *responsesTx) Flush() {
	if f, ok := t.dst.(http.Flusher); ok {
		f.Flush()
	}
}

// flagStream records which wire the attempt speaks. It runs before the
// provider writes anything.
func (t *responsesTx) flagStream() {
	if !t.stream {
		t.stream = true
	}
}

// fail records an upstream failure for the terminal event.
func (t *responsesTx) fail(err error) { t.failure = err }

// finished closes a reply: the full lifecycle for a stream, the single object
// for a collected body. It is a no-op for a collected body that never produced
// a reply, which the caller has already answered with an error.
func (t *responsesTx) finished() {
	if !t.stream {
		t.renderObject()
		return
	}
	if t.closed {
		return
	}
	t.closed = true
	if t.failure != nil {
		t.emit("response.failed", t.failureObject())
		t.endStream()
		return
	}
	t.emit("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": t.messageID(), "output_index": 0,
		"content_index": 0, "text": t.text.String(),
	})
	t.emit("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": t.messageID(), "output_index": 0,
		"content_index": 0, "part": map[string]any{"type": "output_text", "text": t.text.String(), "annotations": []any{}},
	})
	t.emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0, "item": t.messageItem("completed"),
	})
	t.emit("response.completed", map[string]any{"type": "response.completed", "response": t.object("completed")})
	t.endStream()
}

// parse consumes the Chat Completions SSE bytes the provider writes and emits
// the Responses events they imply.
func (t *responsesTx) parse(data []byte) {
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

func (t *responsesTx) line(line []byte) {
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
				Content string `json:"content"`
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
		// Tool-call deltas are dropped: this gateway does not synthesize
		// function_call items, so a tool-only generation completes with an
		// empty message instead of a corrupt one.
		if choice.FinishReason != nil {
			t.upstreamDone = true
		}
	}
}

// delta emits one text chunk and opens the message lifecycle on the first.
func (t *responsesTx) delta(text string) {
	if !t.committed {
		t.committed = true
		t.emit("response.created", map[string]any{"type": "response.created", "response": t.object("in_progress")})
		t.emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": t.object("in_progress")})
		t.emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0, "item": t.messageItem("in_progress"),
		})
		t.emit("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": t.messageID(), "output_index": 0,
			"content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	t.text.WriteString(text)
	t.emit("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": t.messageID(), "output_index": 0,
		"content_index": 0, "delta": text,
	})
}

func (t *responsesTx) emit(name string, payload map[string]any) {
	payload["type"] = name
	payload["sequence_number"] = t.seq
	t.seq++
	encoded, err := json.Marshal(payload)
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
	t.Flush()
}

func (t *responsesTx) endStream() {
	_, _ = t.dst.Write([]byte("data: [DONE]\n\n"))
	t.Flush()
}

func (t *responsesTx) messageID() string { return "msg_" + t.id[len("resp_"):] }

func (t *responsesTx) messageItem(status string) map[string]any {
	content := []any{}
	if status != "in_progress" || t.text.Len() > 0 {
		content = append(content, map[string]any{"type": "output_text", "text": t.text.String(), "annotations": []any{}})
	}
	return map[string]any{
		"id": t.messageID(), "type": "message", "role": "assistant", "status": status, "content": content,
	}
}

func (t *responsesTx) object(status string) map[string]any {
	output := []any{}
	if status == "completed" {
		output = append(output, t.messageItem("completed"))
	}
	return map[string]any{
		"id": t.id, "object": "response", "created_at": t.created, "status": status,
		"model": t.model, "output": output, "usage": responsesUsage(t.usage),
		"error": nil, "incomplete_details": nil,
		"parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{},
		"temperature": 1, "top_p": 1, "max_output_tokens": nil, "metadata": map[string]any{},
	}
}

func (t *responsesTx) failureObject() map[string]any {
	return map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id": t.id, "object": "response", "created_at": t.created, "status": "failed",
			"model": t.model, "output": []any{},
			"error": map[string]any{"code": "server_error", "message": sanitizeFailure(t.failure)},
		},
	}
}

// renderObject writes the non-streaming Responses object from the collected
// chat body.
func (t *responsesTx) renderObject() {
	var chat struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(t.collected, &chat); err != nil {
		writeResponsesError(t.dst, http.StatusBadGateway, "upstream returned malformed JSON")
		return
	}
	if len(chat.Choices) > 0 {
		t.text.WriteString(chatText(chat.Choices[0].Message.Content))
	}
	if chat.Usage != nil {
		t.usage = provider.TokenUsage{Present: true, InputTokens: chat.Usage.PromptTokens, OutputTokens: chat.Usage.CompletionTokens}
		if chat.Usage.PromptTokensDetails != nil {
			t.usage.CachedTokens = chat.Usage.PromptTokensDetails.CachedTokens
		}
		if chat.Usage.CompletionTokensDetails != nil {
			t.usage.ReasoningTokens = chat.Usage.CompletionTokensDetails.ReasoningTokens
		}
	}
	encoded, err := json.Marshal(t.object("completed"))
	if err != nil {
		writeResponsesError(t.dst, http.StatusBadGateway, "upstream returned malformed JSON")
		return
	}
	t.dst.Header().Set("Content-Type", "application/json")
	t.dst.WriteHeader(http.StatusOK)
	_, _ = t.dst.Write(encoded)
}

// responsesAttempt is what a provider writes into. It buffers a non-streaming
// body until the attempt is proven good, and re-frames a stream as it arrives.
type responsesAttempt struct {
	tx   *responsesTx
	body bytes.Buffer
}

func (a *responsesAttempt) Header() http.Header { return a.tx.Header() }

func (a *responsesAttempt) WriteHeader(int) {}

func (a *responsesAttempt) Write(data []byte) (int, error) {
	if a.tx.stream {
		a.tx.parse(data)
		return len(data), nil
	}
	return a.body.Write(data)
}

func (a *responsesAttempt) Flush() {}

// finish keeps the combo loop's failover rules: an attempt only counts as
// committed once a reply exists, so a tool-only or empty upstream still fails
// over. Streaming commits with the first event.
func (a *responsesAttempt) finish(err error) error {
	if a.tx.stream {
		if err != nil {
			if a.tx.committed {
				return errors.Join(errResponseCommitted, err)
			}
			return err
		}
		if !a.tx.upstreamDone {
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

func responsesUsage(usage provider.TokenUsage) map[string]any {
	return map[string]any{
		"input_tokens":          usage.InputTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": usage.CachedTokens},
		"output_tokens":         usage.OutputTokens,
		"output_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens},
		"total_tokens":          usage.InputTokens + usage.OutputTokens,
	}
}

func chatText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	}
	return ""
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}

// writeResponsesError writes the Responses error envelope. Chat keeps its own
// envelope: the two APIs document different shapes.
func writeResponsesError(w http.ResponseWriter, status int, msg string) {
	kind := "invalid_request_error"
	if status >= http.StatusInternalServerError {
		kind = "server_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": kind, "code": statusToCode(status), "param": nil},
	})
}
