package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// messagesGateway wires the real router against a fake upstream. The caller
// authenticates with x-api-key, the header Claude Code sends.
func messagesGateway(t *testing.T, cfg *config.Config, upstream http.Handler) (*httptest.Server, func(t *testing.T, body string) *http.Response, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	provider.Register("openai", provider.NewOpenAI)
	for i := range cfg.Providers {
		if cfg.Providers[i].BaseURL == "" || strings.HasPrefix(cfg.Providers[i].BaseURL, "https://x") {
			cfg.Providers[i].BaseURL = srv.URL
		}
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	gw := httptest.NewServer(testRouter(t, cfg, v))
	t.Cleanup(gw.Close)

	post := func(t *testing.T, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages?beta=true", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("x-api-key", raw)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	return gw, post, &calls
}

type anthropicEvent struct {
	Type    string          `json:"type"`
	Index   int             `json:"index"`
	Message json.RawMessage `json:"message"`
	Content json.RawMessage `json:"content_block"`
	Delta   json.RawMessage `json:"delta"`
	Usage   json.RawMessage `json:"usage"`
	Error   json.RawMessage `json:"error"`
}

func readAnthropicEvents(t *testing.T, body string) []anthropicEvent {
	t.Helper()
	var events []anthropicEvent
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			payload, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var event anthropicEvent
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				t.Fatalf("event %q: %v", payload, err)
			}
			events = append(events, event)
		}
	}
	return events
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		body.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return body.String()
}

func messageTypes(events []anthropicEvent) []string {
	types := make([]string, 0, len(events))
	for _, e := range events {
		if e.Type != "ping" {
			types = append(types, e.Type)
		}
	}
	return types
}

func TestMessagesNonStreamingReturnsAnthropicObject(t *testing.T) {
	cfg := responsesConfig("gpt-4o")
	_, post, _ := messagesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("upstream body: %v", err)
		}
		if body["stream"] == true {
			t.Errorf("upstream stream = true for a non-streaming request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionBody))
	}))

	resp := post(t, `{"model":"openai/gpt-4o","max_tokens":64,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	var object struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&object); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if object.Type != "message" || object.Role != "assistant" || !strings.HasPrefix(object.ID, "msg_") {
		t.Fatalf("object = %+v", object)
	}
	if len(object.Content) != 1 || object.Content[0].Type != "text" || object.Content[0].Text != "hello" {
		t.Fatalf("content = %+v", object.Content)
	}
	if object.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q", object.StopReason)
	}
	if object.Usage.InputTokens != 7 || object.Usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", object.Usage)
	}
}

func TestMessagesStreamEmitsAnthropicLifecycle(t *testing.T) {
	cfg := responsesConfig("gpt-4o")
	_, post, _ := messagesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("upstream stream = %v", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"he"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"llo"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
		} {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))

	resp := post(t, `{"model":"openai/gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	events := readAnthropicEvents(t, readAll(t, resp))
	want := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	got := messageTypes(events)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	var delta struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(events[2].Delta, &delta)
	var second struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(events[3].Delta, &second)
	if delta.Text+second.Text != "hello" {
		t.Fatalf("text deltas = %q", delta.Text+second.Text)
	}
	var messageDelta struct {
		StopReason string `json:"stop_reason"`
	}
	_ = json.Unmarshal(events[5].Delta, &messageDelta)
	if messageDelta.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q", messageDelta.StopReason)
	}
}

func TestMessagesStreamTranslatesToolCalls(t *testing.T) {
	cfg := responsesConfig("gpt-4o")
	_, post, _ := messagesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get","arguments":""}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		} {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))

	resp := post(t, `{"model":"openai/gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	events := readAnthropicEvents(t, readAll(t, resp))

	var blockStart struct {
		Type  string `json:"type"`
		ID    string `json:"id"`
		Name  string `json:"name"`
		Input any    `json:"input"`
	}
	var partial strings.Builder
	var stopReason string
	for _, event := range events {
		switch event.Type {
		case "content_block_start":
			_ = json.Unmarshal(event.Content, &blockStart)
		case "content_block_delta":
			var delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			}
			_ = json.Unmarshal(event.Delta, &delta)
			if delta.Type != "input_json_delta" {
				t.Fatalf("delta type = %q", delta.Type)
			}
			partial.WriteString(delta.PartialJSON)
		case "message_delta":
			var d struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(event.Delta, &d)
			stopReason = d.StopReason
		}
	}
	if blockStart.Type != "tool_use" || blockStart.ID != "call_1" || blockStart.Name != "get" {
		t.Fatalf("content_block_start = %+v", blockStart)
	}
	if partial.String() != `{"q":"x"}` {
		t.Fatalf("partial_json = %q", partial.String())
	}
	if stopReason != "tool_use" {
		t.Fatalf("stop_reason = %q", stopReason)
	}
}

// A stream that fails before any content still opened with SSE headers and a
// ping goroutine, so the failure must be an error event, not a JSON body, and
// the goroutine must stop.
func TestMessagesDirectStreamFailureBeforeContentEmitsErrorEvent(t *testing.T) {
	cfg := responsesConfig("gpt-4o")
	_, post, _ := messagesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	resp := post(t, `{"model":"openai/gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	events := readAnthropicEvents(t, readAll(t, resp))
	if len(events) != 1 || events[0].Type != "error" {
		t.Fatalf("events = %v, want one error event", messageTypes(events))
	}
}

func TestMessagesStreamFailureAfterCommitEmitsError(t *testing.T) {
	cfg := responsesConfig("gpt-4o")
	_, post, _ := messagesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n"))
		flusher.Flush()
		// Close without a finish_reason or [DONE]: an incomplete stream.
	}))

	resp := post(t, `{"model":"openai/gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	events := readAnthropicEvents(t, readAll(t, resp))
	if len(events) == 0 || events[len(events)-1].Type != "error" {
		t.Fatalf("events = %v, want a trailing error event", messageTypes(events))
	}
	var errBody struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(events[len(events)-1].Error, &errBody.Error)
	if errBody.Error.Type == "" || errBody.Error.Message == "" {
		t.Fatalf("error = %+v", errBody.Error)
	}
}

func TestMessagesErrorEnvelopeIsAnthropicShaped(t *testing.T) {
	cfg := responsesConfig("gpt-4o")
	_, post, calls := messagesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))

	resp := post(t, `{"model":"openai/missing","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d for an unknown model", int(calls.Load()))
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Type != "error" || env.Error.Type != "not_found_error" || !strings.Contains(env.Error.Message, "missing") {
		t.Fatalf("envelope = %+v", env)
	}

	resp = post(t, `{"model":"openai/gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusInternalServerError {
		// The upstream status is passed through so the client's retry policy
		// still sees a 500, not a rewritten 502.
		t.Fatalf("upstream failure status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The provider's error carries the status, not the upstream body, so the
	// message is the sanitized provider error text.
	if env.Type != "error" || env.Error.Type != "api_error" || !strings.Contains(env.Error.Message, "500") {
		t.Fatalf("upstream envelope = %+v", env)
	}
}

// A combo target that fails after it committed reaches the client through both
// the responder and the handler. It must emit one error event, not panic on a
// second close.
func TestMessagesComboCommittedFailureEmitsOneError(t *testing.T) {
	cfg := responsesConfig("gpt-4o")
	_, post, _ := messagesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n"))
		flusher.Flush()
	}))
	// A combo of one target whose stream ends without a finish_reason.
	cfg.Combos = []config.Combo{{Name: "auto", Strategy: "priority", Targets: []config.ComboTarget{
		{Provider: "openai", Model: "gpt-4o"},
	}}}

	resp := post(t, `{"model":"auto","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	events := readAnthropicEvents(t, readAll(t, resp))
	errors := 0
	for _, event := range events {
		if event.Type == "error" {
			errors++
		}
	}
	if errors != 1 {
		t.Fatalf("error events = %d, want 1 (events: %v)", errors, messageTypes(events))
	}
}

// An exhausted combo writes an OpenAI-shaped error into the shared writer; the
// client must still see the Anthropic envelope.
func TestMessagesComboExhaustedReturnsAnthropicEnvelope(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{"bad"}}},
		Combos: []config.Combo{{Name: "auto", Strategy: "priority", Targets: []config.ComboTarget{
			{Provider: "openai", Model: "bad"},
		}}},
	}
	_, post, _ := messagesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	resp := post(t, `{"model":"auto","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	var env map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env["type"] != "error" {
		t.Fatalf("combo error body is not Anthropic-shaped: %v", env)
	}
	if _, ok := env["error"].(map[string]any); !ok {
		t.Fatalf("missing error object: %v", env)
	}
}

func TestMessagesComboFailsOverBeforeFirstEvent(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{"bad", "gpt-4o"}}},
		Combos: []config.Combo{{Name: "auto", Strategy: "priority", Targets: []config.ComboTarget{
			{Provider: "openai", Model: "bad"}, {Provider: "openai", Model: "gpt-4o"},
		}}},
	}
	_, post, _ := messagesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model == "bad" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		} {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))

	resp := post(t, `{"model":"auto","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	events := readAnthropicEvents(t, readAll(t, resp))
	got := messageTypes(events)
	if len(got) == 0 || got[0] != "message_start" || got[len(got)-1] != "message_stop" {
		t.Fatalf("events = %v", got)
	}
	for _, event := range events {
		if event.Type == "error" {
			t.Fatalf("error event on the clean fallback stream: %v", got)
		}
	}
}
