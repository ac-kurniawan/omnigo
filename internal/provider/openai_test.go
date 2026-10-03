package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newOpenAI(t *testing.T, upstream http.Handler) Provider {
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	store := staticStore{Credentials{APIKey: "sk-test"}}
	return NewOpenAI(Config{Name: "openai", BaseURL: srv.URL}, store)
}

func TestOpenAIChatRewritesModelInForwardedBody(t *testing.T) {
	var gotModel string
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		gotModel = body.Model
		w.Write([]byte(`{}`))
	}))
	req := ChatRequest{
		Model: "routers9/deepseek-v4-flash-0731",
		Raw:   []byte(`{"model":"myrouter/routers9/deepseek-v4-flash-0731","messages":[]}`),
	}
	if err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder()); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if gotModel != "routers9/deepseek-v4-flash-0731" {
		t.Fatalf("upstream model = %q, want bare resolved id", gotModel)
	}
}

func TestOpenAIChatForwardsUnchangedRawBody(t *testing.T) {
	raw := []byte("{\"model\":\"gpt-4o\",\"temperature\":0.10,\"messages\":[{\"role\":\"user\",\"content\":\"hi\",\"name\":\"Ada\"}],\"stream\":false}")
	var got []byte
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		got, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		w.Write([]byte(`{}`))
	}))
	req := ChatRequest{Model: "gpt-4o", Raw: raw, Parsed: ParseBody(raw)}
	if err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder()); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("upstream body = %s, want original raw bytes %s", got, raw)
	}
}

func TestOpenAIChatRewrittenModelPreservesOtherFields(t *testing.T) {
	raw := []byte(`{"model":"openai/gpt-4o","temperature":0.10,"messages":[{"role":"user","content":"see the developer notes"}],"stream":false}`)
	var got map[string]any
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Write([]byte(`{}`))
	}))
	req := ChatRequest{Model: "gpt-4o", Raw: raw, Parsed: ParseBody(raw)}
	if err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder()); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if got["model"] != "gpt-4o" {
		t.Fatalf("model = %#v, want resolved id", got["model"])
	}
	if got["temperature"] != 0.10 {
		t.Fatalf("temperature = %#v", got["temperature"])
	}
	messages, _ := got["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", got["messages"])
	}
	message, _ := messages[0].(map[string]any)
	if message["role"] != "user" || message["content"] != "see the developer notes" {
		t.Fatalf("message = %#v", message)
	}
}

func TestOpenAIChatRewritesDeveloperMessageRole(t *testing.T) {
	raw := []byte(`{"model":"gpt-4o","messages":[{"role":"developer","content":"instructions"},{"role":"user","content":"a developer said hi"}]}`)
	var got map[string]any
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Write([]byte(`{}`))
	}))
	req := ChatRequest{Model: "gpt-4o", Raw: raw, Parsed: ParseBody(raw)}
	if err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder()); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	messages, _ := got["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", got["messages"])
	}
	first, _ := messages[0].(map[string]any)
	second, _ := messages[1].(map[string]any)
	if first["role"] != "system" || first["content"] != "instructions" {
		t.Fatalf("developer role = %#v, want system", first)
	}
	if second["role"] != "user" || second["content"] != "a developer said hi" {
		t.Fatalf("user message = %#v", second)
	}
}

func TestOpenAIChatForwardsMultimodalPartsUnchanged(t *testing.T) {
	var gotContent []any
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content []any `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		if len(body.Messages) > 0 {
			gotContent = body.Messages[0].Content
		}
		w.Write([]byte(`{}`))
	}))
	req := ChatRequest{
		Model: "gpt-4o",
		Raw:   []byte(`{"model":"openai/gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`),
	}
	if err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder()); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if len(gotContent) != 2 {
		t.Fatalf("upstream content = %#v, want 2 parts", gotContent)
	}
	imagePart, ok := gotContent[1].(map[string]any)
	if !ok {
		t.Fatalf("upstream part = %#v, want object", gotContent[1])
	}
	imageURL, ok := imagePart["image_url"].(map[string]any)
	if !ok || imageURL["url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("upstream image part = %#v, want original image_url", imagePart)
	}
}

func TestOpenAIChatUpstreamErrorReturnsError(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	rec := httptest.NewRecorder()
	req := ChatRequest{
		Model: "nonexistent",
		Raw:   []byte(`{"model":"nonexistent","messages":[]}`),
	}
	err := p.ChatCompletion(context.Background(), req, rec)
	if err == nil {
		t.Fatal("expected error on upstream non-200 status")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("recorder body = %q, expected empty", rec.Body.String())
	}
}

func TestOpenAIChatStreamExceedsClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: chunk1\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// The total stream outlives Timeout; only silence is bounded, so a gap
		// shorter than the idle window must not abort the generation.
		for i := 0; i < 3; i++ {
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write([]byte("data: chunk\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte("data: chunk2\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	p := NewOpenAI(Config{
		Name:    "openai",
		BaseURL: srv.URL,
		Timeout: 50 * time.Millisecond,
	}, staticStore{Credentials{APIKey: "sk-test"}})

	rec := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), ChatRequest{Model: "gpt-4o", Stream: true}, rec)
	if err != nil {
		t.Fatalf("ChatCompletion failed unexpectedly: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "chunk2") {
		t.Fatalf("expected chunk2 in stream, got %q", rec.Body.String())
	}
}

func TestOpenAIChatStreamStallAborts(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: chunk1\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		<-release
	}))
	defer srv.Close()
	defer close(release)

	p := NewOpenAI(Config{
		Name:    "openai",
		BaseURL: srv.URL,
		Timeout: 50 * time.Millisecond,
	}, staticStore{Credentials{APIKey: "sk-test"}})

	rec := httptest.NewRecorder()
	done := make(chan error, 1)
	go func() {
		done <- p.ChatCompletion(context.Background(), ChatRequest{Model: "gpt-4o", Stream: true}, rec)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrUpstreamStall) {
			t.Fatalf("err = %v, want ErrUpstreamStall", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("silent upstream did not abort the stream")
	}
}

func TestOpenAITimeoutConfigured(t *testing.T) {
	store := staticStore{Credentials{APIKey: "sk-test"}}
	p1 := NewOpenAI(Config{Name: "openai", BaseURL: "https://example.com", Timeout: 15 * time.Second}, store)
	op1, ok := p1.(*openAIProvider)
	if !ok || op1.client.Timeout != 15*time.Second {
		t.Fatalf("client.Timeout = %v, want 15s", op1.client.Timeout)
	}

	// Default when unset or <= 0
	p2 := NewOpenAI(Config{Name: "openai", BaseURL: "https://example.com"}, store)
	op2, ok := p2.(*openAIProvider)
	if !ok || op2.client.Timeout != 60*time.Second {
		t.Fatalf("client.Timeout = %v, want default 60s", op2.client.Timeout)
	}
}

// A stream that keeps producing bytes never trips the idle bound, so without a
// budget a trickling upstream can hold the generation open forever.
func TestOpenAIStreamBudgetEndsTricklingStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for {
			select {
			case <-release:
				return
			default:
			}
			_, _ = w.Write([]byte("data: chunk\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer srv.Close()
	defer close(release)

	p := NewOpenAI(Config{
		Name:          "openai",
		BaseURL:       srv.URL,
		Timeout:       5 * time.Second,
		StreamTimeout: 150 * time.Millisecond,
	}, staticStore{Credentials{APIKey: "sk-test"}})
	rec := httptest.NewRecorder()
	done := make(chan error, 1)
	go func() {
		done <- p.ChatCompletion(context.Background(), ChatRequest{Model: "gpt-4o", Stream: true}, rec)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrUpstreamStall) {
			t.Fatalf("err = %v, want ErrUpstreamStall for the exhausted stream budget", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("trickling stream was not ended by its budget")
	}
}

// A budget of zero is the explicit opt-out: generated text may run as long as
// it keeps producing bytes.
func TestOpenAIStreamBudgetZeroIsUnbounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 6; i++ {
			_, _ = w.Write([]byte("data: chunk\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(30 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	p := NewOpenAI(Config{
		Name:    "openai",
		BaseURL: srv.URL,
		Timeout: 50 * time.Millisecond,
	}, staticStore{Credentials{APIKey: "sk-test"}})

	rec := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), ChatRequest{Model: "gpt-4o", Stream: true}, rec); err != nil {
		t.Fatalf("unbounded stream failed: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "chunk") {
		t.Fatalf("stream body = %q", rec.Body.String())
	}
}

// A non-streaming request must keep its configured wall-clock deadline: a
// trickling upstream cannot be bounded by inter-byte silence alone, or it
// holds the request (and a concurrency slot) open indefinitely. A stream is
// the opposite: its total duration is unbounded by design.
func TestOpenAINonStreamingUsesDeadlineClient(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		// Dribble bytes forever, never completing the JSON response.
		for i := 0; i < 50; i++ {
			_, _ = w.Write([]byte(" "))
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-release:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()
	defer close(release)

	p := NewOpenAI(Config{
		Name:    "openai",
		BaseURL: srv.URL,
		Timeout: 100 * time.Millisecond,
	}, staticStore{Credentials{APIKey: "sk-test"}})

	start := time.Now()
	rec := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), ChatRequest{Model: "gpt-4o", Stream: false}, rec)
	if err == nil {
		t.Fatal("non-streaming request against a dribbling upstream succeeded; its deadline was dropped")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("non-streaming request ran %s before failing, want the 100ms configured deadline", elapsed)
	}
}

// The gateway is a proxy, not a transparent tunnel: forwarding upstream
// headers verbatim would leak session cookies, upstream auth challenges, and
// internal infrastructure metadata to API clients.
func TestOpenAIStripsSensitiveUpstreamHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "session=upstream-internal; Path=/")
		w.Header().Set("WWW-Authenticate", "Basic realm=internal")
		w.Header().Set("X-Internal-Debug", "upstream-node-7")
		w.Header().Set("X-Request-Id", "req-123")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := NewOpenAI(Config{Name: "openai", BaseURL: srv.URL, Timeout: 5 * time.Second}, staticStore{Credentials{APIKey: "sk-test"}})
	rec := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), ChatRequest{Model: "gpt-4o", Stream: true}, rec); err != nil {
		t.Fatalf("chat completion: %v", err)
	}
	for _, h := range []string{"Set-Cookie", "WWW-Authenticate", "X-Internal-Debug"} {
		if v := rec.Header().Get(h); v != "" {
			t.Errorf("upstream header %s forwarded to client: %q", h, v)
		}
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Error("Content-Type was stripped; clients need it to parse the stream")
	}
}

type staticStore struct{ c Credentials }

func (s staticStore) Get() Credentials      { return s.c }
func (s staticStore) Put(Credentials) error { return nil }

func TestOpenAIModels(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`))
	}))
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "gpt-4o" {
		t.Fatalf("models = %+v", models)
	}
}

func TestOpenAIModelsCapabilities(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"grok-4.7"},{"id":"deepseek-v4.1-flash"},{"id":"o3-mini"},{"id":"o1"},{"id":"gpt-4o"}]}`))
	}))
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Model{}
	for _, model := range models {
		byID[model.ID] = model
	}
	for _, id := range []string{"grok-4.7", "deepseek-v4.1-flash", "o3-mini", "o1"} {
		got := byID[id].Capabilities
		if got == nil || !got.Reasoning || got.DefaultEffort != "medium" || len(got.ReasoningEfforts) != 3 || got.ReasoningEfforts[0] != "low" || got.ReasoningEfforts[1] != "medium" || got.ReasoningEfforts[2] != "high" {
			t.Fatalf("%s capabilities = %+v", id, got)
		}
	}
	if byID["gpt-4o"].Capabilities != nil {
		t.Fatalf("gpt-4o capabilities = %+v, want nil", byID["gpt-4o"].Capabilities)
	}
	if CapabilitiesFor("openai", "openai-gpt-4o") != nil {
		t.Fatal("openai-gpt-4o matched the o1 prefix")
	}
}

func TestOpenAIChatStreamsPassthrough(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("auth = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	rec := httptest.NewRecorder()
	req := ChatRequest{Model: "gpt-4o", Stream: true, Messages: []Message{{Role: "user", Content: "hi"}}}
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	got := rec.Body.String()
	want := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	if got != want {
		t.Fatalf("proxied body = %q, want %q", got, want)
	}
}

// Framing must survive the proxy untouched, including CR line endings. A
// rewrite would break clients that split on the terminator the upstream sent.
func TestOpenAIStreamFramingPassesThroughUnchanged(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\r\ndata: [DONE]\r\n"
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	rec := httptest.NewRecorder()
	req := ChatRequest{Model: "gpt-4o", Stream: true, Messages: []Message{{Role: "user", Content: "hi"}}}
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if got := rec.Body.String(); got != body {
		t.Fatalf("proxied body = %q, want %q", got, body)
	}
}

// A stream that closes without any finish signal must not be reported as
// success: the client would treat the truncated (or empty) body as a finished
// answer. Both "finish_reason" on a chunk and a [DONE] frame count as
// completion signals; a bare EOF does not.
func TestOpenAIStreamWithoutFinishSignalIsIncomplete(t *testing.T) {
	cases := map[string]string{
		"empty body":      "",
		"keepalive only":  ": keepalive\n\n",
		"content no stop": "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"truncated":       "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"tial\"}}]}\n\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(body))
			}))
			rec := httptest.NewRecorder()
			req := ChatRequest{Model: "gpt-4o", Stream: true, Messages: []Message{{Role: "user", Content: "hi"}}}
			if err := p.ChatCompletion(context.Background(), req, rec); err == nil {
				t.Fatalf("stream without a finish signal reported success; body = %q", rec.Body.String())
			}
		})
	}
}

// Usage-only final chunks carry "choices": [] with no finish_reason, so the
// completeness check must accept a [DONE] frame as the end-of-stream signal
// and must not require the last data frame to be a completion chunk.
func TestOpenAIStreamDoneFrameIsComplete(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"total_tokens\":3}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	rec := httptest.NewRecorder()
	req := ChatRequest{Model: "gpt-4o", Stream: true, Messages: []Message{{Role: "user", Content: "hi"}}}
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// A finish_reason on any chunk completes the stream even when upstream omits
// the [DONE] sentinel; some compatible providers end the response with the
// finish chunk alone.
func TestOpenAIStreamFinishReasonIsComplete(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
	}))
	rec := httptest.NewRecorder()
	req := ChatRequest{Model: "gpt-4o", Stream: true, Messages: []Message{{Role: "user", Content: "hi"}}}
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
}

// Non-streaming responses are complete JSON bodies, not SSE: the completeness
// scan must not reject a body that happens to contain SSE-shaped text.
func TestOpenAINonStreamBodyUnaffected(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"data: hi"},"finish_reason":"stop"}]}`))
	}))
	rec := httptest.NewRecorder()
	req := ChatRequest{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hi"}}}
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "data: hi") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestOpenAITest(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	}))
	start := time.Now()
	res := p.Test(context.Background())
	if !res.OK {
		t.Fatalf("Test = %+v", res)
	}
	if res.LatencyMS < 0 || res.LatencyMS > time.Since(start).Milliseconds()+5 {
		t.Fatalf("latency = %dms", res.LatencyMS)
	}
}

// Every upstream 4xx/5xx must leave a warning in the operator log carrying the
// upstream error message, redacted, not just the status code.
func TestOpenAIChatLogsWarningOnUpstreamError(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"boom from upstream: token=abc123secret"}}`))
	}))
	rec := httptest.NewRecorder()
	req := ChatRequest{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hi"}}}
	err := p.ChatCompletion(context.Background(), req, rec)
	var status interface{ HTTPStatus() int }
	if !errors.As(err, &status) || status.HTTPStatus() != http.StatusBadGateway {
		t.Fatalf("err = %v, want HTTPStatusError 502", err)
	}
	if err.Error() != "upstream status 502" {
		t.Fatalf("err = %q, want unchanged status-only message", err)
	}
	out := buf.String()
	for _, want := range []string{"[warn] upstream status=502", `provider="openai"`, `model="gpt-4o"`, "boom from upstream"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q, got: %s", want, out)
		}
	}
	if strings.Contains(out, "abc123secret") {
		t.Fatalf("log leaks upstream credential: %s", out)
	}
}
