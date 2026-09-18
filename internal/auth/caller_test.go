package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// The instrumentation middleware wrapping the mux owns the request it was
// handed, so a caller identity recorded inside Middleware must be visible to
// that outer middleware after the request completes. A context value installed
// by Middleware would not be: it would only reach handlers nested below it.
func TestMiddlewareRecordsCallerIdentity(t *testing.T) {
	raw, hash, prefix, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store := vault.NewMemoryStore(&vault.Vault{
		ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}},
	})

	caller := &Caller{}
	var seenInsideHandler string
	h := Middleware(store.Get)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenInsideHandler = CallerFrom(r.Context()).ID()
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	req = req.WithContext(WithCaller(req.Context(), caller))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := caller.ID(); got != "k1" {
		t.Fatalf("outer caller ID = %q, want %q", got, "k1")
	}
	if seenInsideHandler != "k1" {
		t.Fatalf("handler caller ID = %q, want %q", seenInsideHandler, "k1")
	}
}

// A rejected request must leave the identity empty: the label is recorded only
// after the key hash validated, so a client cannot choose an unknown key to
// mint a series of its own.
func TestMiddlewareLeavesCallerEmptyOnInvalidKey(t *testing.T) {
	raw, hash, prefix, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	_ = raw
	store := vault.NewMemoryStore(&vault.Vault{
		ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}},
	})

	caller := &Caller{}
	h := Middleware(store.Get)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not run for an invalid key")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer ak-ATTACK-KEY")
	req = req.WithContext(WithCaller(req.Context(), caller))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if got := caller.ID(); got != "" {
		t.Fatalf("caller ID = %q, want empty for a rejected key", got)
	}
}

// Middleware is also used with no Caller installed (direct handlers, helper
// tests); recording must be a no-op rather than a nil dereference.
func TestMiddlewareWithoutCallerInContextDoesNotPanic(t *testing.T) {
	raw, hash, prefix, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store := vault.NewMemoryStore(&vault.Vault{
		ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}},
	})

	h := Middleware(store.Get)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}
