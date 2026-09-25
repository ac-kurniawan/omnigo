package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// sseCommitState watches a streaming combo body for the first reply. Comment
// lines, pings, a role frame, reasoning, and an empty stop are not one: a
// finished stream with none can still fail over. Once assistant text or a tool
// call is complete, the bytes already read are committed so the client loses
// nothing.
type sseCommitState struct {
	scanned int
	event   string
	data    []byte
	partial []byte
	ready   bool
}

func (s *sseCommitState) feed(body []byte) bool {
	if s.ready {
		return true
	}
	rest := body[s.scanned:]
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			s.partial = append(s.partial, rest...)
			s.scanned = len(body)
			return false
		}
		var line []byte
		if len(s.partial) > 0 {
			s.partial = append(s.partial, rest[:i]...)
			line = s.partial
		} else {
			line = rest[:i]
		}
		rest = rest[i+1:]
		s.scanned = len(body) - len(rest)
		if sseLineEndsEvent(line) && sseContentEvent(s.event, s.data) {
			s.ready = true
			s.partial = nil
			s.scanned = len(body)
			return true
		}
		s.consumeLine(line)
		s.partial = s.partial[:0]
	}
	return false
}

func (s *sseCommitState) consumeLine(line []byte) {
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		s.event, s.data = "", s.data[:0]
		return
	}
	if trimmed[0] == ':' {
		return
	}
	field, value, _ := bytes.Cut(trimmed, []byte(":"))
	value = bytes.TrimLeft(value, " ")
	switch string(field) {
	case "event":
		s.event = string(value)
	case "data":
		if len(s.data) > 0 {
			s.data = append(s.data, '\n')
		}
		s.data = append(s.data, value...)
	}
}

func sseLineEndsEvent(line []byte) bool {
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return len(bytes.TrimSpace(line)) == 0
}

func sseContentEvent(event string, data []byte) bool {
	if ssePingName(event) {
		return false
	}
	payload := bytes.TrimSpace(data)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return false
	}
	if !json.Valid(payload) {
		return false
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return false
	}
	return sseReplyPayload(decoded)
}

// sseReplyPayload reports whether a decoded SSE payload carries assistant text
// or a tool call. A role, reasoning, usage, or an empty finish frame does not:
// those can finish a generation the client still has no reply from.
func sseReplyPayload(payload any) bool {
	switch v := payload.(type) {
	case map[string]any:
		if sseText(v["content"]) || sseToolCall(v) {
			return true
		}
		for _, key := range []string{"choices", "candidates", "delta", "message", "output", "content", "parts"} {
			if sseReplyPayload(v[key]) {
				return true
			}
		}
		return false
	case []any:
		for _, item := range v {
			if sseReplyPayload(item) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func sseText(v any) bool {
	text, ok := v.(string)
	return ok && strings.TrimSpace(text) != ""
}

func sseToolCall(v map[string]any) bool {
	for _, key := range []string{"tool_calls", "tool_use", "function_call"} {
		if present, ok := v[key]; ok && present != nil {
			return true
		}
	}
	return false
}

// chatJSONWithoutReply reports a finished chat completion whose message has
// neither text nor a tool call. Any other JSON body is left alone: not every
// successful response is a chat completion.
func chatJSONWithoutReply(body []byte) bool {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return false
	}
	choices, ok := decoded["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	return !sseReplyPayload(decoded)
}

func ssePingName(name string) bool {
	switch strings.ToLower(name) {
	case "ping", "keepalive", "heartbeat":
		return true
	default:
		return false
	}
}

var errResponseCommitted = errors.New("response already committed")

type bufferedResponseWriter struct {
	destination   http.ResponseWriter
	header        http.Header
	body          bytes.Buffer
	status        int
	stream        bool
	headerWritten bool
	committed     bool
	sse           sseCommitState
}

func newBufferedResponseWriter(destination http.ResponseWriter, stream bool) *bufferedResponseWriter {
	return &bufferedResponseWriter{destination: destination, header: make(http.Header), status: http.StatusOK, stream: stream}
}

func (w *bufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.committed || w.headerWritten {
		return
	}
	w.status = status
	w.headerWritten = true
}

func (w *bufferedResponseWriter) Write(data []byte) (int, error) {
	if w.committed {
		return w.destination.Write(data)
	}
	if !w.headerWritten {
		w.headerWritten = true
	}
	return w.body.Write(data)
}

func (w *bufferedResponseWriter) Flush() {
	if !w.stream || w.status < http.StatusOK || w.status >= http.StatusMultipleChoices {
		return
	}
	if !w.committed {
		if !w.sse.feed(w.body.Bytes()) {
			return
		}
		w.commit()
	}
	// Every flush after the first content frame must still reach the client:
	// the remaining SSE chunks would otherwise sit in the server's buffer until EOF.
	if flusher, ok := w.destination.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *bufferedResponseWriter) commit() {
	if w.committed {
		return
	}
	copyHeader(w.destination.Header(), w.header)
	w.destination.WriteHeader(w.status)
	w.committed = true
	_, _ = w.destination.Write(w.body.Bytes())
	w.body.Reset()
}

// Committed reports whether a content frame has already reached the client.
// Nested attempt writers use it to stop buffering once failover is impossible.
func (w *bufferedResponseWriter) Committed() bool {
	return w.committed
}

func (w *bufferedResponseWriter) finish(err error) error {
	if w.committed {
		if err != nil {
			return errors.Join(errResponseCommitted, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if w.status < http.StatusOK || w.status >= http.StatusMultipleChoices {
		return provider.NewHTTPStatusError(w.status, "")
	}
	if w.body.Len() == 0 {
		return errors.New("upstream returned an empty response")
	}
	if w.stream {
		if !w.sse.feed(w.body.Bytes()) {
			return errors.New("upstream returned no reply")
		}
	} else if strings.Contains(w.header.Get("Content-Type"), "application/json") {
		if !json.Valid(w.body.Bytes()) {
			return errors.New("upstream returned malformed JSON")
		}
		if chatJSONWithoutReply(w.body.Bytes()) {
			return errors.New("upstream returned no reply")
		}
	}
	w.commit()
	return nil
}

func copyHeader(destination, source http.Header) {
	for key := range destination {
		destination.Del(key)
	}
	for key, values := range source {
		destination[key] = append([]string(nil), values...)
	}
}

// commitTracker records whether a response has started reaching the client.
// Providers stream to the raw writer on the direct path, so a failure after the
// first byte must not be reported as a fresh JSON error envelope: that would
// append an error object to an already-delivered SSE body.
type commitTracker struct {
	http.ResponseWriter
	committed bool
}

func (w *commitTracker) WriteHeader(status int) {
	w.committed = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *commitTracker) Write(data []byte) (int, error) {
	w.committed = true
	return w.ResponseWriter.Write(data)
}

func (w *commitTracker) Flush() {
	w.committed = true
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// writeStreamError tells a client whose stream already started that the
// generation did not finish. The response is committed, so failover is no
// longer possible and a JSON envelope would be parsed as a malformed chunk; an
// SSE error event is a frame a streaming client can read and act on instead of
// treating the truncated answer as complete.
func writeStreamError(w http.ResponseWriter, message string) {
	payload, err := json.Marshal(map[string]any{"error": map[string]string{"message": message}})
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("event: error\ndata: "))
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n\n"))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
