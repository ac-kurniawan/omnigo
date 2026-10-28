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

// responsesGateway wires the real router against a fake upstream and returns
// the gateway plus a caller that posts one Responses body.
func responsesGateway(t *testing.T, cfg *config.Config, upstream http.Handler) (*httptest.Server, func(t *testing.T, body string) *http.Response, *atomic.Int32) {
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
		req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+raw)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	return gw, post, &calls
}

func responsesConfig(model string) *config.Config {
	return &config.Config{Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{model}}}}
}

const chatCompletionBody = `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":1}}}`

func TestResponsesNonStreamingReturnsResponsesObject(t *testing.T) {
	cfg := responsesConfig("gpt-4o")
	_, post, _ := responsesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionBody))
	}))

	resp := post(t, `{"model":"openai/gpt-4o","instructions":"be brief","input":"say hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	var object struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Status  string `json:"status"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			TotalTokens        int `json:"total_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&object); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if object.Object != "response" || object.Status != "completed" || object.Model != "openai/gpt-4o" {
		t.Fatalf("object = %+v", object)
	}
	if len(object.Output) != 1 || object.Output[0].Type != "message" || object.Output[0].Role != "assistant" || object.Output[0].Status != "completed" {
		t.Fatalf("output = %+v", object.Output)
	}
	content := object.Output[0].Content
	if len(content) != 1 || content[0].Type != "output_text" || content[0].Text != "hello" {
		t.Fatalf("content = %+v", content)
	}
	if object.Usage.InputTokens != 7 || object.Usage.OutputTokens != 3 || object.Usage.TotalTokens != 10 ||
		object.Usage.InputTokensDetails.CachedTokens != 2 || object.Usage.OutputTokensDetails.ReasoningTokens != 1 {
		t.Fatalf("usage = %+v", object.Usage)
	}
}

type responsesEvent struct {
	Type           string          `json:"type"`
	SequenceNumber int             `json:"sequence_number"`
	Delta          string          `json:"delta"`
	Text           string          `json:"text"`
	Response       json.RawMessage `json:"response"`
}

// readResponsesEvents parses the gateway SSE body into its events.
func readResponsesEvents(t *testing.T, body string) []responsesEvent {
	t.Helper()
	var events []responsesEvent
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var event responsesEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("event %q: %v", payload, err)
		}
		events = append(events, event)
	}
	return events
}

func TestResponsesStreamEmitsFullLifecycle(t *testing.T) {
	cfg := responsesConfig("gpt-4o")
	_, post, _ := responsesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	resp := post(t, `{"model":"openai/gpt-4o","input":"hi","stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		body.Write(buf[:n])
		if err != nil {
			break
		}
	}

	want := []string{
		"response.created", "response.in_progress", "response.output_item.added",
		"response.content_part.added", "response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.completed",
	}
	events := readResponsesEvents(t, body.String())
	if len(events) != len(want) {
		t.Fatalf("events = %d (%v), want %d", len(events), eventTypes(events), len(want))
	}
	for i, typ := range want {
		if events[i].Type != typ {
			t.Fatalf("event %d = %q, want %q (sequence: %v)", i, events[i].Type, typ, eventTypes(events))
		}
		if events[i].SequenceNumber != i+1 {
			t.Fatalf("event %d sequence = %d, want %d", i, events[i].SequenceNumber, i+1)
		}
	}
	if got := events[4].Delta + events[5].Delta; got != "hello" {
		t.Fatalf("deltas = %q", got)
	}
	if events[6].Text != "hello" {
		t.Fatalf("output_text.done text = %q", events[6].Text)
	}
	var completed struct {
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(events[9].Response, &completed); err != nil {
		t.Fatalf("completed: %v", err)
	}
	if len(completed.Output) == 0 || completed.Output[0].Content[0].Text != "hello" {
		t.Fatalf("completed output = %+v", completed.Output)
	}
	if completed.Usage.InputTokens != 7 || completed.Usage.OutputTokens != 3 {
		t.Fatalf("completed usage = %+v", completed.Usage)
	}
	if !strings.HasSuffix(body.String(), "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with [DONE]: %q", body.String())
	}
}

func eventTypes(events []responsesEvent) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}

func TestResponsesStreamFailureAfterCommitEmitsFailed(t *testing.T) {
	cfg := responsesConfig("gpt-4o")
	_, post, _ := responsesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n"))
		flusher.Flush()
		// Close without a finish_reason or [DONE]: an incomplete stream.
	}))

	resp := post(t, `{"model":"openai/gpt-4o","input":"hi","stream":true}`)
	var body strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		body.Write(buf[:n])
		if err != nil {
			break
		}
	}
	events := readResponsesEvents(t, body.String())
	if len(events) < 2 || events[len(events)-1].Type != "response.failed" {
		t.Fatalf("events = %v, want response.failed last", eventTypes(events))
	}
	if !strings.HasSuffix(body.String(), "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with [DONE]")
	}
	var failed struct {
		Response struct {
			Status string `json:"status"`
			Error  struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(events[len(events)-1].Response, &failed.Response); err != nil {
		t.Fatalf("failed event: %v", err)
	}
	if failed.Response.Status != "failed" || failed.Response.Error.Code != "server_error" || failed.Response.Error.Message == "" {
		t.Fatalf("failed = %+v", failed)
	}
}

func TestResponsesComboFailsOverBeforeFirstEvent(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{"bad", "gpt-4o"}}},
		Combos: []config.Combo{{Name: "auto", Strategy: "priority", Targets: []config.ComboTarget{
			{Provider: "openai", Model: "bad"}, {Provider: "openai", Model: "gpt-4o"},
		}}},
	}
	_, post, _ := responsesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	resp := post(t, `{"model":"auto","input":"hi","stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		body.Write(buf[:n])
		if err != nil {
			break
		}
	}
	events := readResponsesEvents(t, body.String())
	if len(events) == 0 || events[0].Type != "response.created" || events[len(events)-1].Type != "response.completed" {
		t.Fatalf("events = %v", eventTypes(events))
	}
	for _, event := range events {
		if event.Type == "response.failed" {
			t.Fatalf("failed event on the clean fallback stream: %v", eventTypes(events))
		}
	}
}

func TestResponsesRejectsStatefulField(t *testing.T) {
	cfg := responsesConfig("gpt-4o")
	_, post, calls := responsesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream was called for a rejected request")
	}))

	resp := post(t, `{"model":"openai/gpt-4o","input":"hi","previous_response_id":"resp_1"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Error.Message != "responses: previous_response_id is not supported by this gateway" {
		t.Fatalf("message = %q", envelope.Error.Message)
	}
	if envelope.Error.Type != "invalid_request_error" || envelope.Error.Code != "invalid_request_error" {
		t.Fatalf("error = %+v", envelope.Error)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestResponsesDirectModelNotFound(t *testing.T) {
	cfg := responsesConfig("gpt-4o")
	_, post, _ := responsesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	resp := post(t, `{"model":"openai/nope","input":"hi"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Error.Message != "model not found: openai/nope" || envelope.Error.Code != "model_not_found" {
		t.Fatalf("error = %+v", envelope.Error)
	}
}

func TestResponsesSmartComboRoutes(t *testing.T) {
	srv := systemOneServer(t, "pro", 0.95, nil)
	useSystemOneServer(t, srv.URL)
	cfg := smartConfig()
	var gotModel atomic.Value
	_, post, _ := responsesGateway(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel.Store(body.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionBody))
	}))

	resp := post(t, `{"model":"combo/smart","input":"design a hard system"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if model, _ := gotModel.Load().(string); model != "gpt-4o" {
		t.Fatalf("routed model = %q, want the pro target", model)
	}
}
