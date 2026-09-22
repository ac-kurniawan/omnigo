package antigravity

import (
	"context"
	"encoding/json"
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
		w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hel\"}]}}]}\n\n"))
		w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"lo\"}]}}]}\n\n"))
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
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"success\"}]}}]}\n\n"))
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

func TestChatAccountPoolRetriesLimitedAccountOnNextRequest(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		calls = append(calls, token)
		if token == "first" && len(calls) == 1 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"success\"}]}}]}\n\n"))
	}))
	defer srv.Close()

	store := &antigravityPoolStore{accounts: []provider.Credentials{
		{AccessToken: "first", AccountID: "google-1", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
		{AccessToken: "second", AccountID: "google-2", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, store)
	req := provider.ChatRequest{Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	for range 2 {
		rr := httptest.NewRecorder()
		if err := p.ChatCompletion(context.Background(), req, rr); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rr.Body.String(), "success") {
			t.Fatalf("body = %s", rr.Body.String())
		}
	}
	// The 429 fails over inside the first request, then the next request tries
	// the previously limited account again instead of remembering the cooldown.
	if len(calls) != 3 || calls[0] != "first" || calls[1] != "second" || calls[2] != "first" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestChatAllAccountsRateLimitedFailsOverEachRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if calls.Load() <= 2 {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"success\"}]}}]}\n\n"))
	}))
	defer srv.Close()

	store := &antigravityPoolStore{accounts: []provider.Credentials{
		{AccessToken: "first", AccountID: "google-1", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
		{AccessToken: "second", AccountID: "google-2", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, store)
	req := provider.ChatRequest{Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}}}

	if err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder()); err == nil || err.Error() != "antigravity: rate limited" {
		t.Fatalf("first request error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("first request upstream calls = %d, want one per account", calls.Load())
	}

	// The next request starts over: the previously limited accounts are tried
	// again, so a transient 429 cannot hide the pool.
	rr := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), req, rr); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if calls.Load() != 3 || !strings.Contains(rr.Body.String(), "success") {
		t.Fatalf("calls = %d, body = %q", calls.Load(), rr.Body.String())
	}
}

func TestChatSingleAccount429RetriesOnNextRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	store := staticStore{provider.Credentials{AccessToken: "token", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, store)
	req := provider.ChatRequest{Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	for range 2 {
		err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder())
		if err == nil || err.Error() != "antigravity: rate limited" {
			t.Fatalf("ChatCompletion error = %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (no remembered cooldown)", calls.Load())
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
			err := newRateLimitError(headers).(*rateLimitError)
			if err.Cooldown() != tt.want {
				t.Fatalf("cooldown = %s, want %s", err.Cooldown(), tt.want)
			}
		})
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

func TestChatNonStreamingReturnsOpenAIJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hel\"}]}}]}\n\n"))
		w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"lo\"}]}}]}\n\n"))
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
		w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"success!\"}]}}]}\n\n"))
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
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hel\"}]}}]}\n\n"))
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
