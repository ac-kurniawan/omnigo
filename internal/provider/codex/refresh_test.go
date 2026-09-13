package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type memoryCredStore struct {
	mu    sync.Mutex
	creds provider.Credentials
	puts  int
}

func (s *memoryCredStore) Get() provider.Credentials {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creds
}

func (s *memoryCredStore) Put(creds provider.Credentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds = creds
	s.puts++
	return nil
}

func TestRefreshOmitsScopeAndPersistsRotation(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	idToken := testJWT(t, map[string]any{
		"email":                       "new@example.com",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "workspace-new"},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["scope"]; ok {
			t.Fatal("refresh request contained scope")
		}
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "refresh-old" || body["client_id"] != ClientID {
			t.Fatalf("body = %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-new",
			"refresh_token": "refresh-new",
			"id_token":      idToken,
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	tokenURL = server.URL

	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Minute)}}
	manager := NewTokenManager(store)
	creds, err := manager.EnsureFreshToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "access-new" || creds.RefreshToken != "refresh-new" || creds.IDToken != idToken || creds.Email != "new@example.com" || creds.AccountID != "workspace-new" {
		t.Fatalf("credentials = %+v", creds)
	}
	stored := store.Get()
	if stored.AccessToken != "access-new" || stored.RefreshToken != "refresh-new" || store.puts != 1 {
		t.Fatalf("stored = %+v, puts = %d", stored, store.puts)
	}
}

func TestParallelRefreshUsesOneUpstreamRequest(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(25 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-new",
			"refresh_token": "refresh-new",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	tokenURL = server.URL

	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Minute)}}
	manager := NewTokenManager(store)
	const workers = 20
	start := make(chan struct{})
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			creds, err := manager.EnsureFreshToken(context.Background())
			if err == nil && creds.AccessToken != "access-new" {
				err = &unexpectedTokenError{got: creds.AccessToken}
			}
			errCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

type unexpectedTokenError struct{ got string }

func (e *unexpectedTokenError) Error() string { return "unexpected access token: " + e.got }

func TestForceRefreshSerializesAndSkipsChangedCredentials(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(25 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-new",
			"refresh_token": "refresh-new",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	tokenURL = server.URL

	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Hour)}}
	manager := NewTokenManager(store)
	const workers = 20
	start := make(chan struct{})
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := manager.ForceRefreshToken(context.Background(), "access-old")
			errCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestRefreshPersistsRotatedTokenWhenIDTokenInvalid(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-new",
			"refresh_token": "refresh-new",
			"id_token":      "not-a-jwt",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	tokenURL = server.URL

	store := &memoryCredStore{creds: provider.Credentials{
		AccessToken:  "access-old",
		RefreshToken: "refresh-old",
		ExpiresAt:    time.Now().Add(time.Minute),
		Email:        "old@example.com",
		AccountID:    "workspace-old",
	}}
	manager := NewTokenManager(store)
	creds, err := manager.EnsureFreshToken(context.Background())
	if err != nil {
		t.Fatalf("refresh error = %v", err)
	}
	if creds.AccessToken != "access-new" || creds.RefreshToken != "refresh-new" {
		t.Fatalf("credentials = %+v", creds)
	}
	if creds.Email != "old@example.com" || creds.AccountID != "workspace-old" {
		t.Fatalf("identity changed on parse failure: %+v", creds)
	}
	stored := store.Get()
	if stored.AccessToken != "access-new" || stored.RefreshToken != "refresh-new" || store.puts != 1 {
		t.Fatalf("rotated credentials not persisted: %+v, puts = %d", stored, store.puts)
	}
}

func TestRefreshErrorLeavesCredentialsUnchanged(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","refresh_token":"refresh-old"}`))
	}))
	defer server.Close()
	tokenURL = server.URL

	original := provider.Credentials{AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Minute)}
	store := &memoryCredStore{creds: original}
	manager := NewTokenManager(store)
	if _, err := manager.EnsureFreshToken(context.Background()); err == nil {
		t.Fatal("expected refresh error")
	}
	if got := store.Get(); got.AccessToken != original.AccessToken || got.RefreshToken != original.RefreshToken || store.puts != 0 {
		t.Fatalf("stored = %+v, puts = %d", got, store.puts)
	}
}
