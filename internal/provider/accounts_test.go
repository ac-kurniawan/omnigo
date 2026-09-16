package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type accountTestStore struct {
	accounts []Credentials
}

func (s *accountTestStore) Get() Credentials {
	if len(s.accounts) == 0 {
		return Credentials{}
	}
	return s.accounts[0]
}

func (s *accountTestStore) Put(c Credentials) error { return s.PutAccount(c.Identity(), c) }
func (s *accountTestStore) Accounts() []Credentials { return append([]Credentials(nil), s.accounts...) }
func (s *accountTestStore) PutAccount(identity string, c Credentials) error {
	for i := range s.accounts {
		if s.accounts[i].Identity() == identity {
			s.accounts[i] = c
			return nil
		}
	}
	return errors.New("not found")
}

type accountCooldownError time.Duration

func (e accountCooldownError) Error() string           { return "failed" }
func (e accountCooldownError) Cooldown() time.Duration { return time.Duration(e) }
func (e accountCooldownError) DrainReason() string     { return e.Error() }

func TestAccountPoolPreservesOrderAndCooldown(t *testing.T) {
	now := time.Unix(1_000, 0)
	pool := AccountPool{}
	pool.SetClock(func() time.Time { return now })
	store := &accountTestStore{accounts: []Credentials{{AccountID: "one"}, {AccountID: "two"}}}
	accounts := pool.Available(store)
	if len(accounts) != 2 || accounts[0].AccountID != "one" || accounts[1].AccountID != "two" {
		t.Fatalf("accounts = %+v", accounts)
	}
	pool.MarkFailed(accounts[0], accountCooldownError(2*time.Minute))
	accounts, reason := pool.AvailableWithError(store)
	if len(accounts) != 1 || accounts[0].AccountID != "two" {
		t.Fatalf("healthy accounts = %+v", accounts)
	}
	if reason == nil || reason.Error() != "failed" {
		t.Fatalf("reason = %v, want failed", reason)
	}
	withCooldown, ok := reason.(interface{ Cooldown() time.Duration })
	if !ok {
		t.Fatalf("reason type = %T, want cooldown error", reason)
	}
	if withCooldown.Cooldown() != 2*time.Minute {
		t.Fatalf("cooldown = %s, want 2m", withCooldown.Cooldown())
	}
	now = now.Add(2*time.Minute + time.Second)
	accounts, reason = pool.AvailableWithError(store)
	if len(accounts) != 2 || accounts[0].AccountID != "one" {
		t.Fatalf("accounts after cooldown = %+v", accounts)
	}
	if reason != nil {
		t.Fatalf("reason after cooldown = %v, want nil", reason)
	}
}

func TestAttemptWriterStreamingFlushesImmediatelyOn200(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewStreamingAttemptWriter(rec, true)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	// Before write, nothing committed
	if w.Committed() {
		t.Fatal("expected not committed before write")
	}

	// Write chunk and flush
	_, err := w.Write([]byte("data: hello\n\n"))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	w.Flush()

	// Now committed and destination contains data
	if !w.Committed() {
		t.Fatal("expected committed after 200 OK flush")
	}
	if rec.Body.String() != "data: hello\n\n" {
		t.Fatalf("destination body = %q, want %q", rec.Body.String(), "data: hello\n\n")
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("destination Content-Type = %q", rec.Header().Get("Content-Type"))
	}

	// Write next chunk
	_, _ = w.Write([]byte("data: world\n\n"))
	w.Flush()
	if rec.Body.String() != "data: hello\n\ndata: world\n\n" {
		t.Fatalf("destination body after 2nd chunk = %q", rec.Body.String())
	}

	// Final Commit is a no-op when already committed
	if err := w.Commit(rec); err != nil {
		t.Fatalf("Commit error: %v", err)
	}
	if rec.Body.String() != "data: hello\n\ndata: world\n\n" {
		t.Fatalf("destination body after Commit = %q", rec.Body.String())
	}
}

func TestAttemptWriterBuffersOnError(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewStreamingAttemptWriter(rec, true)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	w.Flush()

	if w.Committed() {
		t.Fatal("expected NOT committed on 401 error")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("destination should be empty on error, got %q", rec.Body.String())
	}
}

// A scoped store backed by a plain (non-pooled) store keeps its own copy of the
// credentials, which concurrent refreshes read and write. Reads must never
// observe a torn write: a stale AccessToken makes token managers believe a
// refresh already happened and re-refresh against a rotating token.
func TestScopedStoreConcurrentGetPut(t *testing.T) {
	base := &plainTestStore{}
	s := ScopedStore(base, Credentials{AccessToken: "old", RefreshToken: "r"})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				got := s.Get()
				if got.AccessToken != "old" && got.AccessToken != "new" {
					t.Errorf("torn read: AccessToken = %q", got.AccessToken)
					return
				}
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if err := s.Put(Credentials{AccessToken: "new", RefreshToken: "r2"}); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

type plainTestStore struct {
	mu    sync.Mutex
	creds Credentials
}

func (s *plainTestStore) Get() Credentials {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creds
}

func (s *plainTestStore) Put(c Credentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds = c
	return nil
}
