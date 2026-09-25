package antigravity

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type staticStore struct{ c provider.Credentials }

func (s staticStore) Get() provider.Credentials      { return s.c }
func (s staticStore) Put(provider.Credentials) error { return nil }

type antigravityPoolStore struct {
	accounts []provider.Credentials
}

func (s *antigravityPoolStore) Get() provider.Credentials {
	if len(s.accounts) == 0 {
		return provider.Credentials{}
	}
	return s.accounts[0]
}

func (s *antigravityPoolStore) Put(c provider.Credentials) error {
	return s.PutAccount(c.Identity(), c)
}

func (s *antigravityPoolStore) Accounts() []provider.Credentials {
	return append([]provider.Credentials(nil), s.accounts...)
}

func (s *antigravityPoolStore) PutAccount(identity string, c provider.Credentials) error {
	for i := range s.accounts {
		if s.accounts[i].Identity() == identity {
			s.accounts[i] = c
			return nil
		}
	}
	return nil
}

type mutableStore struct {
	c provider.Credentials
}

func (s *mutableStore) Get() provider.Credentials { return s.c }
func (s *mutableStore) Put(c provider.Credentials) error {
	s.c = c
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type closeTrackingBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *closeTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestChatCompletionClosesUpstreamResponseBody(t *testing.T) {
	saved := UnaryClient().Transport
	t.Cleanup(func() { SetHTTPTransport(saved) })
	body := &closeTrackingBody{Reader: strings.NewReader(`data: {"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}` + "\n\n")}
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	p := New(provider.Config{Name: "agy", BaseURL: "https://example.invalid", Transport: transport}, staticStore{provider.Credentials{AccessToken: "token", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}}}, httptest.NewRecorder())
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if !body.closed.Load() {
		t.Fatal("upstream response body was not closed")
	}
}

// A stream that ends without a finish reason was cut off: the upstream closed
// or reset before the generation completed. Reporting success and appending
// [DONE] makes the client treat the truncated answer as final, so the gateway
// must fail the attempt instead.
func TestStreamEndsWithoutFinishReasonIsIncomplete(t *testing.T) {
	saved := UnaryClient().Transport
	t.Cleanup(func() { SetHTTPTransport(saved) })
	body := io.NopCloser(strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n"))
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	p := New(provider.Config{Name: "agy", BaseURL: "https://example.invalid", Transport: transport},
		staticStore{provider.Credentials{AccessToken: "token", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})

	rr := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{
		Model: "gemini", Stream: true, Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, rr)
	if err == nil {
		t.Fatal("stream ending without a finish reason was reported as success")
	}
	if strings.Contains(rr.Body.String(), "data: [DONE]") {
		t.Fatalf("truncated stream was closed with [DONE]: %s", rr.Body.String())
	}
}

// A mid-stream connection reset is the same failure: bytes already delivered,
// then the socket dies. It must surface as an error, not a finished answer.
func TestStreamReaderErrorIsIncomplete(t *testing.T) {
	saved := UnaryClient().Transport
	t.Cleanup(func() { SetHTTPTransport(saved) })
	body := io.NopCloser(io.MultiReader(
		strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n"),
		errReader{errors.New("connection reset by peer")},
	))
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	p := New(provider.Config{Name: "agy", BaseURL: "https://example.invalid", Transport: transport},
		staticStore{provider.Credentials{AccessToken: "token", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})

	err := p.ChatCompletion(context.Background(), provider.ChatRequest{
		Model: "gemini", Stream: true, Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, httptest.NewRecorder())
	if err == nil {
		t.Fatal("stream that reset mid-way was reported as success")
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// The non-streaming path aggregates the same SSE feed, so a feed that closes
// before a finish reason is a truncated answer too, not a short completion.
func TestCompleteEndsWithoutFinishReasonIsIncomplete(t *testing.T) {
	saved := UnaryClient().Transport
	t.Cleanup(func() { SetHTTPTransport(saved) })
	body := io.NopCloser(strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n"))
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	p := New(provider.Config{Name: "agy", BaseURL: "https://example.invalid", Transport: transport},
		staticStore{provider.Credentials{AccessToken: "token", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})

	err := p.ChatCompletion(context.Background(), provider.ChatRequest{
		Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, httptest.NewRecorder())
	if err == nil {
		t.Fatal("completion ending without a finish reason was reported as success")
	}
}

// The last Gemini frame often carries only the finish reason. Dropping it
// leaves the client with content chunks whose finish_reason is null and no
// signal that the generation ended, other than [DONE].
func TestStreamFinishOnlyFrameReachesClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hel\"}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1,\"totalTokenCount\":4}}\n\n"))
	}))
	defer srv.Close()

	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, staticStore{provider.Credentials{AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})
	rec := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{
		Model: "gemini", Stream: true, Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, rec)
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("terminal chunk missing finish_reason: %s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":3`) {
		t.Fatalf("terminal chunk dropped usage: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("finished stream missing [DONE]: %s", body)
	}
}

func TestStreamAcceptsDataFieldWithoutSpace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data:{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer srv.Close()

	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, staticStore{provider.Credentials{AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})
	rec := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{
		Model: "gemini", Stream: true, Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, rec)
	if err != nil {
		t.Fatalf("space-less data field was dropped: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// A thinking-only generation has no candidate text. Treating that as a
// truncated stream fails a request the upstream completed.
func TestStreamThoughtOnlyIsComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"thought\":true,\"text\":\"because\"}]},\"finishReason\":\"STOP\"}]}}\n\n"))
	}))
	defer srv.Close()

	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, staticStore{provider.Credentials{AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})
	rec := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{
		Model: "gemini", Stream: true, Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, rec)
	if err != nil {
		t.Fatalf("thought-only stream failed: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"reasoning_content":"because"`) {
		t.Fatalf("thought dropped: %s", rec.Body.String())
	}
}

func TestCompleteMapsMaxTokensAndUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"cut\"}]},\"finishReason\":\"MAX_TOKENS\"}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":2,\"totalTokenCount\":10}}\n\n"))
	}))
	defer srv.Close()

	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, staticStore{provider.Credentials{AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})
	rec := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{
		Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, rec)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != "cut" || body.Choices[0].FinishReason != "length" {
		t.Fatalf("choices = %+v", body.Choices)
	}
	if body.Usage.PromptTokens != 8 || body.Usage.CompletionTokens != 2 || body.Usage.TotalTokens != 10 {
		t.Fatalf("usage = %+v", body.Usage)
	}
}

func TestCompleteFunctionCallIsToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"weather\",\"args\":{\"city\":\"Rome\"}}}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer srv.Close()

	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, staticStore{provider.Credentials{AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})
	rec := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{
		Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, rec)
	if err != nil {
		t.Fatalf("function-call completion failed: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"name":"weather"`) || !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestDiscoverProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "loadCodeAssist") {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"cloudaicompanionProject":{"id":"proj-42"}}`))
	}))
	defer srv.Close()
	baseURL = srv.URL

	pid, err := DiscoverProject(context.Background(), "tok")
	if err != nil {
		t.Fatalf("DiscoverProject: %v", err)
	}
	if pid != "proj-42" {
		t.Fatalf("pid = %q", pid)
	}
}

func TestDiscoverProjectStringField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "loadCodeAssist") {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"cloudaicompanionProject":"proj-string-99"}`))
	}))
	defer srv.Close()
	baseURL = srv.URL

	pid, err := DiscoverProject(context.Background(), "tok")
	if err != nil {
		t.Fatalf("DiscoverProject: %v", err)
	}
	if pid != "proj-string-99" {
		t.Fatalf("pid = %q", pid)
	}
}

func TestDiscoverProjectWithOnboardRetry(t *testing.T) {
	var loadCalls int32
	var onboardCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "loadCodeAssist") {
			count := atomic.AddInt32(&loadCalls, 1)
			if count == 1 {
				// First call: empty project
				w.Write([]byte(`{}`))
				return
			}
			// Second call after onboarding: project discovered!
			w.Write([]byte(`{"cloudaicompanionProject":{"id":"proj-onboarded-77"}}`))
			return
		}
		if strings.Contains(r.URL.Path, "onboardUser") {
			atomic.AddInt32(&onboardCalls, 1)
			w.Write([]byte(`{"done":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	baseURL = srv.URL

	pid, err := DiscoverProject(context.Background(), "tok")
	if err != nil {
		t.Fatalf("DiscoverProject: %v", err)
	}
	if pid != "proj-onboarded-77" {
		t.Fatalf("pid = %q", pid)
	}
	if atomic.LoadInt32(&onboardCalls) != 1 {
		t.Fatalf("onboardCalls = %d, want 1", atomic.LoadInt32(&onboardCalls))
	}
}

func TestModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "fetchAvailableModels") {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"models":[{"name":"gemini-3.7-flash-medium"},{"name":"claude-sonnet-4-6"}]}`))
	}))
	defer srv.Close()

	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, staticStore{provider.Credentials{AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "gemini-3.7-flash-medium" {
		t.Fatalf("models = %+v", models)
	}
}

func TestModelsAutoRefreshesExpiredToken(t *testing.T) {
	// Upstream token endpoint for Refresh
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Fatalf("grant_type = %s", r.Form.Get("grant_type"))
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "new-tok-99", "expires_in": 3600})
	}))
	defer tokenSrv.Close()
	tokenURL = tokenSrv.URL

	// Upstream Google API
	var usedToken string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usedToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Write([]byte(`{"models":[{"name":"gemini-3.7-flash-medium"}]}`))
	}))
	defer apiSrv.Close()

	store := &mutableStore{c: provider.Credentials{
		AccessToken:  "expired-tok",
		RefreshToken: "valid-rt",
		ExpiresAt:    time.Now().Add(-10 * time.Minute), // EXPIRED!
		ProjectID:    "proj-1",
	}}
	p := New(provider.Config{Name: "agy", BaseURL: apiSrv.URL}, store)

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %+v", models)
	}
	if usedToken != "new-tok-99" {
		t.Fatalf("usedToken = %q, want new-tok-99", usedToken)
	}
	if store.c.AccessToken != "new-tok-99" {
		t.Fatalf("stored accessToken not refreshed: %q", store.c.AccessToken)
	}
}

func TestChatStreamsOpenAISSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hel\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
		w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"lo\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer srv.Close()

	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, staticStore{provider.Credentials{AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})
	rec := httptest.NewRecorder()
	req := provider.ChatRequest{Model: "gemini-3.7-flash-medium", Stream: true, Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hel") || !strings.Contains(body, "lo") {
		t.Fatalf("body = %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE]: %q", body)
	}
}

func TestChatAccountPoolFallsBackOnPreCommitFailure(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		calls = append(calls, token)
		if token == "first" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream down"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"success\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer srv.Close()

	store := &antigravityPoolStore{accounts: []provider.Credentials{
		{AccessToken: "first", AccountID: "google-1", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
		{AccessToken: "second", AccountID: "google-2", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, store)
	rr := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gemini", Stream: true, Messages: []provider.Message{{Role: "user", Content: "hi"}}}, rr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Body.String(), "success") {
		t.Fatalf("body = %s", rr.Body.String())
	}
	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestChatAccountPoolFallsBackAfter429AndRespectsRetryAfter(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		calls = append(calls, token)
		if token == "first" {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"success\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer srv.Close()

	store := &antigravityPoolStore{accounts: []provider.Credentials{
		{AccessToken: "first", AccountID: "google-1", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
		{AccessToken: "second", AccountID: "google-2", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, store).(*Provider)
	p.pool.SetClock(func() time.Time { return now })
	req := provider.ChatRequest{Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	for range 2 {
		rr := httptest.NewRecorder()
		if err := p.ChatCompletion(context.Background(), req, rr); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rr.Body.String(), "success") {
			t.Fatalf("body = %s", rr.Body.String())
		}
		now = now.Add(61 * time.Second)
	}
	if len(calls) != 3 || calls[0] != "first" || calls[1] != "second" || calls[2] != "second" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestChatAllAccountsCoolingReturnsRateLimitUntilCooldownExpires(t *testing.T) {
	start := time.Unix(2_000_000_000, 0)
	now := start
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if now.Equal(start) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"success\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer srv.Close()

	store := &antigravityPoolStore{accounts: []provider.Credentials{
		{AccessToken: "first", AccountID: "google-1", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
		{AccessToken: "second", AccountID: "google-2", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, store).(*Provider)
	p.pool.SetClock(func() time.Time { return now })
	req := provider.ChatRequest{Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}}}

	for range 2 {
		err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder())
		if err == nil || err.Error() != "antigravity: rate limited" {
			t.Fatalf("ChatCompletion error = %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls during cooldown = %d, want 2", calls.Load())
	}

	now = now.Add(time.Minute + time.Second)
	rr := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), req, rr); err != nil {
		t.Fatalf("ChatCompletion after cooldown: %v", err)
	}
	if calls.Load() != 3 || !strings.Contains(rr.Body.String(), "success") {
		t.Fatalf("calls = %d, body = %q", calls.Load(), rr.Body.String())
	}
}

func TestChatSingleAccountCoolingReturnsRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	store := staticStore{provider.Credentials{AccessToken: "token", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, store).(*Provider)
	req := provider.ChatRequest{Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	for range 2 {
		err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder())
		if err == nil || err.Error() != "antigravity: rate limited" {
			t.Fatalf("ChatCompletion error = %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestChatWithoutCredentialsReturnsNotAuthenticated(t *testing.T) {
	p := New(provider.Config{Name: "agy"}, staticStore{}).(*Provider)
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{}, httptest.NewRecorder())
	if err == nil || err.Error() != "antigravity: not authenticated" {
		t.Fatalf("ChatCompletion error = %v", err)
	}
}

func TestRateLimitCooldown(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter string
		want       time.Duration
	}{
		{name: "missing", want: defaultRateLimitCooldown},
		{name: "invalid", retryAfter: "later", want: defaultRateLimitCooldown},
		{name: "below minimum", retryAfter: "1", want: minRateLimitCooldown},
		{name: "valid", retryAfter: "120", want: 2 * time.Minute},
		{name: "above maximum", retryAfter: "3600", want: maxRateLimitCooldown},
		{name: "overflow safe", retryAfter: "9223372036854775807", want: maxRateLimitCooldown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := make(http.Header)
			headers.Set("Retry-After", tt.retryAfter)
			err := newRateLimitError(headers, "").(*rateLimitError)
			if err.Cooldown() != tt.want {
				t.Fatalf("cooldown = %s, want %s", err.Cooldown(), tt.want)
			}
		})
	}
}

func TestRateLimitErrorPassesThrough429(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Retry-After", "120")
	err := newRateLimitError(headers, "")

	var status interface{ HTTPStatus() int }
	if !errors.As(err, &status) || status.HTTPStatus() != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus missing or wrong on %T", err)
	}
	var retry interface{ RetryAfter() time.Duration }
	if !errors.As(err, &retry) || retry.RetryAfter() != 2*time.Minute {
		t.Fatalf("RetryAfter missing or wrong on %T: %v", err, err)
	}
}

func TestRateLimitErrorExposesOnlyStructuredIdentifiers(t *testing.T) {
	secret := "ya29.secret-token-value"
	err := newRateLimitError(make(http.Header), `[{"error":{"message":"quota: `+secret+` hit","status":"RESOURCE_EXHAUSTED","errors":[{"reason":"rateLimitExceeded"}]}}]`)
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "quota:") {
		t.Fatalf("error exposes upstream message: %v", err)
	}
	if !strings.Contains(err.Error(), "RESOURCE_EXHAUSTED (reason: rateLimitExceeded)") {
		t.Fatalf("error drops safe upstream identifiers: %v", err)
	}
	if err.(*rateLimitError).DrainReason() != err.Error() {
		t.Fatalf("drain reason = %q, want %q", err.(*rateLimitError).DrainReason(), err.Error())
	}
}

func TestRateLimitErrorRejectsUnknownStructuredIdentifiers(t *testing.T) {
	secret := "ya29.secret-token-value"
	err := newRateLimitError(make(http.Header), `[{"error":{"status":"`+secret+`","errors":[{"reason":"`+secret+`"}]}}]`)
	if strings.Contains(err.Error(), secret) || err.Error() != "antigravity: rate limited" {
		t.Fatalf("error exposes unknown upstream identifier: %v", err)
	}
}

func TestRateLimitErrorEmptyBodyKeepsBaseMessage(t *testing.T) {
	err := newRateLimitError(make(http.Header), "")
	if err.Error() != "antigravity: rate limited" {
		t.Fatalf("error = %v", err)
	}
}

func TestRetryAfterLabelRejectsUntrustedHeaderText(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Retry-After", "bad\nforged-log-entry")
	if got := retryAfterLabel(headers); got != "invalid" {
		t.Fatalf("label = %q, want invalid", got)
	}
}

func TestChatRateLimitedLogsUpstreamDetail(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	upstreamSecret := "upstream-secret-value-XYZ"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`[{"error":{"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED","errors":[{"reason":"rateLimitExceeded"}],"token":"` + upstreamSecret + `"}}]`))
	}))
	defer srv.Close()

	accessToken := "access-secret-value-XYZ"
	store := staticStore{provider.Credentials{AccessToken: accessToken, AccountID: "google-1", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, store)
	req := provider.ChatRequest{Model: "gemini-3.8-flash-tiered", Messages: []provider.Message{{Role: "user", Content: "hi"}}}

	err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder())
	if err == nil || !strings.Contains(err.Error(), "RESOURCE_EXHAUSTED") {
		t.Fatalf("error = %v, want upstream detail", err)
	}
	out := buf.String()
	for _, want := range []string{"agy", "google-1", "gemini-3.8-flash-tiered", "retry-after=42s", "RESOURCE_EXHAUSTED"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q, got: %s", want, out)
		}
	}
	for _, secret := range []string{accessToken, upstreamSecret} {
		if strings.Contains(out, secret) || strings.Contains(err.Error(), secret) {
			t.Fatalf("rate-limit diagnostics leak secret %q: log=%s err=%v", secret, out, err)
		}
	}
}

func TestRateLimitAccountLabelNeverUsesEmail(t *testing.T) {
	if got := rateLimitAccountLabel(provider.Credentials{Email: "private@example.com"}); got != "configured-account" {
		t.Fatalf("label = %q, want non-PII fallback", got)
	}
	if got := rateLimitAccountLabel(provider.Credentials{AccountID: "account-1", Email: "private@example.com"}); got != "account-1" {
		t.Fatalf("label = %q, want account ID", got)
	}
}

func TestChatAccountPoolExhaustsEachAccountOnceAfter429(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	store := &antigravityPoolStore{accounts: []provider.Credentials{
		{AccessToken: "first", AccountID: "google-1", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
		{AccessToken: "second", AccountID: "google-2", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
		{AccessToken: "third", AccountID: "google-3", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, store)
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}}}, httptest.NewRecorder())
	if err == nil || err.Error() != "antigravity: rate limited" {
		t.Fatalf("error = %v", err)
	}
	if len(calls) != 3 || calls[0] != "first" || calls[1] != "second" || calls[2] != "third" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestChat429OnOneModelLeavesAccountAvailableForAnother(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, body.Model)
		if body.Model == "gemini" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer srv.Close()

	store := &antigravityPoolStore{accounts: []provider.Credentials{
		{AccessToken: "only", AccountID: "google-1", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, store)
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}}}, httptest.NewRecorder()); err == nil {
		t.Fatal("gemini request succeeded, want rate limit")
	}
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "claude", Messages: []provider.Message{{Role: "user", Content: "hi"}}}, httptest.NewRecorder()); err != nil {
		t.Fatalf("claude request = %v, want the account still available", err)
	}
	if len(calls) != 2 || calls[0] != "gemini" || calls[1] != "claude" {
		t.Fatalf("calls = %v, want both models attempted on the same account", calls)
	}
}

func TestChat401RemovesAccountForEveryModel(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, body.Model)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	store := &antigravityPoolStore{accounts: []provider.Credentials{
		{AccessToken: "only", AccountID: "google-1", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, store)
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}}}, httptest.NewRecorder()); err == nil {
		t.Fatal("gemini request succeeded, want auth failure")
	}
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "claude", Messages: []provider.Message{{Role: "user", Content: "hi"}}}, httptest.NewRecorder()); err == nil {
		t.Fatal("claude request reached upstream after an account-wide auth failure")
	}
	if len(calls) != 1 || calls[0] != "gemini" {
		t.Fatalf("calls = %v, want only the first model", calls)
	}
}

func TestChatNonStreamingReturnsOpenAIJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hel\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
		w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"lo\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer srv.Close()

	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, staticStore{provider.Credentials{AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})
	rec := httptest.NewRecorder()
	req := provider.ChatRequest{Model: "gemini-3.7-flash-medium", Stream: false, Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, rec.Body.String())
	}
	if body.Object != "chat.completion" || body.Model != req.Model {
		t.Fatalf("response metadata = %+v", body)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Role != "assistant" || body.Choices[0].Message.Content != "hello" || body.Choices[0].FinishReason != "stop" {
		t.Fatalf("choices = %+v", body.Choices)
	}
	if body.Usage.PromptTokens != 0 || body.Usage.CompletionTokens != 0 || body.Usage.TotalTokens != 0 {
		t.Fatalf("usage = %+v", body.Usage)
	}
}

func TestChatRetriesOn401(t *testing.T) {
	// Token refresh server
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "retried-tok-55", "expires_in": 3600})
	}))
	defer tokenSrv.Close()
	tokenURL = tokenSrv.URL

	var attempts int32
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnt := atomic.AddInt32(&attempts, 1)
		if cnt == 1 {
			// First call: 401 Unauthorized
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":401,"message":"UNAUTHENTICATED"}}`))
			return
		}
		// Second call after refresh: success stream!
		if got := r.Header.Get("Authorization"); got != "Bearer retried-tok-55" {
			t.Fatalf("retry auth = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"success!\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer apiSrv.Close()

	store := &mutableStore{c: provider.Credentials{
		AccessToken:  "stale-tok",
		RefreshToken: "rt-123",
		ExpiresAt:    time.Now().Add(time.Hour), // nominally valid, but server rejects with 401
		ProjectID:    "proj-1",
	}}
	p := New(provider.Config{Name: "agy", BaseURL: apiSrv.URL}, store)
	rec := httptest.NewRecorder()
	req := provider.ChatRequest{Model: "gemini-3.7-flash-medium", Stream: true, Messages: []provider.Message{{Role: "user", Content: "hi"}}}

	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", atomic.LoadInt32(&attempts))
	}
	if !strings.Contains(rec.Body.String(), "success!") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// Streaming must reach the client as the upstream produces it. Full buffering
// would withhold every byte until the upstream closed, so a probe reading the
// client writer while the upstream stream is still open must already see the
// first translated chunk.
func TestChatStreamsBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hel\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)

	p := New(provider.Config{Name: "agy", BaseURL: srv.URL, Timeout: 5 * time.Second}, staticStore{provider.Credentials{AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})
	rec := newProbeRecorder()
	done := make(chan error, 1)
	go func() {
		done <- p.ChatCompletion(context.Background(), provider.ChatRequest{
			Model: "gemini-3.7-flash-medium", Stream: true,
			Messages: []provider.Message{{Role: "user", Content: "hi"}},
		}, rec)
	}()

	deadline := time.After(2 * time.Second)
	for rec.delivered() == "" {
		select {
		case <-deadline:
			t.Fatal("no bytes reached the client while the upstream stream was still open: response is fully buffered")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if !strings.Contains(rec.delivered(), "hel") {
		t.Fatalf("delivered bytes = %q, want the first translated chunk", rec.delivered())
	}
}

// probeRecorder captures what a streaming client would have observed before the
// upstream stream finished.
type probeRecorder struct {
	header http.Header
	mu     sync.Mutex
	body   strings.Builder
}

func newProbeRecorder() *probeRecorder {
	return &probeRecorder{header: make(http.Header)}
}

func (r *probeRecorder) Header() http.Header { return r.header }
func (r *probeRecorder) WriteHeader(int)     {}
func (r *probeRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(b)
}
func (r *probeRecorder) Flush() {}
func (r *probeRecorder) delivered() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}
