package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type testMemoryCredStore struct {
	mu    sync.Mutex
	creds provider.Credentials
	puts  int
}

func (s *testMemoryCredStore) Get() provider.Credentials {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creds
}

func (s *testMemoryCredStore) Put(creds provider.Credentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds = creds
	s.puts++
	return nil
}

func TestRefreshOmitsScopeAndPersistsRotation(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Fatalf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("refresh_token") != "rt-old" {
			t.Fatalf("refresh_token = %q", r.Form.Get("refresh_token"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-new",
			"refresh_token": "rt-rotated",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	tokenURL = server.URL

	store := &testMemoryCredStore{creds: provider.Credentials{
		AccessToken:  "at-old",
		RefreshToken: "rt-old",
		ProjectID:    "proj-1",
		ExpiresAt:    time.Now().Add(time.Minute),
	}}
	manager := NewTokenManager(store)
	creds, err := manager.EnsureFreshToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "at-new" || creds.RefreshToken != "rt-rotated" {
		t.Fatalf("creds = %+v", creds)
	}
	if store.puts != 1 || store.Get().RefreshToken != "rt-rotated" {
		t.Fatalf("stored = %+v, puts = %d", store.Get(), store.puts)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestParallelRefreshUsesOneUpstreamRequest(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-shared",
			"refresh_token": "rt-shared",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	tokenURL = server.URL

	store := &testMemoryCredStore{creds: provider.Credentials{
		AccessToken:  "at-old",
		RefreshToken: "rt-old",
		ExpiresAt:    time.Now().Add(time.Minute),
	}}
	manager := NewTokenManager(store)

	const workers = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			creds, err := manager.EnsureFreshToken(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if creds.AccessToken != "at-shared" {
				errs <- unexpectedTokenError{got: creds.AccessToken}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
}

type unexpectedTokenError struct{ got string }

func (e unexpectedTokenError) Error() string { return "unexpected access token: " + e.got }

func TestForceRefreshSerializesAndSkipsChangedCredentials(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-force",
			"refresh_token": "rt-force",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	tokenURL = server.URL

	store := &testMemoryCredStore{creds: provider.Credentials{
		AccessToken:  "at-old",
		RefreshToken: "rt-old",
		ExpiresAt:    time.Now().Add(time.Hour),
	}}
	manager := NewTokenManager(store)

	const workers = 6
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			creds, err := manager.ForceRefreshToken(context.Background(), "at-old")
			if err != nil {
				errs <- err
				return
			}
			if creds.AccessToken != "at-force" {
				errs <- unexpectedTokenError{got: creds.AccessToken}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
}

func TestUnrecoverableRefreshFastFailsUntilCredentialsChange(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-new",
			"refresh_token": "refresh-newer",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	tokenURL = server.URL

	store := &testMemoryCredStore{creds: provider.Credentials{
		AccessToken:  "access-old",
		RefreshToken: "refresh-old",
		ExpiresAt:    time.Now().Add(time.Minute),
	}}
	manager := NewTokenManager(store)
	for range 2 {
		_, err := manager.EnsureFreshToken(context.Background())
		if err == nil || err.Error() != "antigravity: re-authentication required; use Connect Google" {
			t.Fatalf("error = %v", err)
		}
		if strings.Contains(err.Error(), "refresh-old") || strings.Contains(err.Error(), "invalid_grant") {
			t.Fatalf("error leaked upstream details: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}

	// Stored credentials get a newer expiry but same dead token pair: still fails fast
	store.mu.Lock()
	store.creds.ExpiresAt = time.Now().Add(time.Hour)
	store.mu.Unlock()
	if _, err := manager.EnsureFreshToken(context.Background()); err == nil || err.Error() != "antigravity: re-authentication required; use Connect Google" {
		t.Fatalf("fresh dead credentials error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d after fresh dead credentials, want 1", got)
	}

	// User re-authenticates (new refresh token): circuit breaker resets and hits endpoint
	store.mu.Lock()
	store.creds.RefreshToken = "refresh-new"
	store.creds.ExpiresAt = time.Now().Add(time.Minute)
	store.mu.Unlock()
	creds, err := manager.EnsureFreshToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "access-new" || calls.Load() != 2 {
		t.Fatalf("credentials = %+v, calls = %d", creds, calls.Load())
	}
}

func TestRefreshTokenReusedRequiresReauthentication(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"refresh_token_reused","message":"do not expose this"}}`))
	}))
	defer server.Close()
	tokenURL = server.URL

	manager := NewTokenManager(&testMemoryCredStore{creds: provider.Credentials{RefreshToken: "dead"}})
	_, err := manager.EnsureFreshToken(context.Background())
	if err == nil || err.Error() != "antigravity: re-authentication required; use Connect Google" {
		t.Fatalf("error = %v", err)
	}
}

func TestChatAutoRefreshesExpiredTokenBeforeStreamRequest(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	var tokenCalls atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Fatalf("grant_type = %q", r.Form.Get("grant_type"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-chat-tok",
			"refresh_token": "fresh-chat-rt",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()
	tokenURL = tokenSrv.URL

	var streamAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"success\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer apiSrv.Close()

	store := &mutableStore{c: provider.Credentials{
		AccessToken:  "expired-chat-tok",
		RefreshToken: "chat-rt",
		ExpiresAt:    time.Now().Add(-10 * time.Minute),
		ProjectID:    "proj-1",
	}}
	p := New(provider.Config{Name: "agy", BaseURL: apiSrv.URL}, store)
	rec := httptest.NewRecorder()
	req := provider.ChatRequest{Model: "gemini-3.7-flash-medium", Stream: true, Messages: []provider.Message{{Role: "user", Content: "hi"}}}

	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token calls = %d, want 1", tokenCalls.Load())
	}
	if streamAuth != "Bearer fresh-chat-tok" {
		t.Fatalf("stream auth = %q, want Bearer fresh-chat-tok", streamAuth)
	}
	if store.c.AccessToken != "fresh-chat-tok" || store.c.RefreshToken != "fresh-chat-rt" {
		t.Fatalf("stored creds not updated: %+v", store.c)
	}
}

func TestChatConcurrentUnauthorizedUsesOneRefresh(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	var refreshCalls atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		time.Sleep(20 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-concurrent-tok",
			"refresh_token": "fresh-concurrent-rt",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()
	tokenURL = tokenSrv.URL

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer old-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":401,"message":"UNAUTHENTICATED"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"success\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer apiSrv.Close()

	store := &mutableStore{c: provider.Credentials{
		AccessToken:  "old-tok",
		RefreshToken: "rt-123",
		ExpiresAt:    time.Now().Add(time.Hour),
		ProjectID:    "proj-1",
	}}
	p := New(provider.Config{Name: "agy", BaseURL: apiSrv.URL, Timeout: 5 * time.Second}, store)

	const workers = 6
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			errs <- p.ChatCompletion(context.Background(), provider.ChatRequest{
				Model: "gemini-3.7-flash-medium", Stream: true,
				Messages: []provider.Message{{Role: "user", Content: "hi"}},
			}, rec)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls.Load())
	}
}

func TestChatUnrecoverableRefreshFastFails(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	var refreshCalls atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
	}))
	defer tokenSrv.Close()
	tokenURL = tokenSrv.URL

	store := &mutableStore{c: provider.Credentials{
		AccessToken:  "expired-tok",
		RefreshToken: "revoked-rt",
		ExpiresAt:    time.Now().Add(-10 * time.Minute),
		ProjectID:    "proj-1",
	}}
	p := New(provider.Config{Name: "agy", BaseURL: "http://unused"}, store).(*Provider)
	now := time.Unix(2_000_000_000, 0)
	p.pool.SetClock(func() time.Time { return now })

	req := provider.ChatRequest{Model: "gemini-3.7-flash-medium", Stream: true, Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder())
	if err == nil || err.Error() != "antigravity: re-authentication required; use Connect Google" {
		t.Fatalf("ChatCompletion err = %v, want reauth required", err)
	}

	// Past the account cooldown the credential is tried again: the breaker must
	// answer from the remembered rejection instead of re-POSTing a dead token.
	now = now.Add(2 * provider.DefaultAccountCooldown)
	err = p.ChatCompletion(context.Background(), req, httptest.NewRecorder())
	if err == nil || err.Error() != "antigravity: re-authentication required; use Connect Google" {
		t.Fatalf("ChatCompletion err after cooldown = %v, want reauth required", err)
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls.Load())
	}
}
