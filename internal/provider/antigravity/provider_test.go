package antigravity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type staticStore struct{ c provider.Credentials }

func (s staticStore) Get() provider.Credentials      { return s.c }
func (s staticStore) Put(provider.Credentials) error { return nil }

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
