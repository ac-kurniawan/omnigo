package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestGenerateKeyPrefixAndLength(t *testing.T) {
	raw, hash, prefix, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "ak-") {
		t.Fatalf("raw key %q missing ak- prefix", raw)
	}
	if !strings.HasPrefix(prefix, "ak-") || len(prefix) != 11 {
		t.Fatalf("prefix %q wrong shape", prefix)
	}
	if len(hash) != 64 {
		t.Fatalf("hash length %d, want 64 hex chars", len(hash))
	}
}

func TestHashKeyDeterministic(t *testing.T) {
	raw, _, _, _ := GenerateKey()
	want := sha256.Sum256([]byte(raw))
	if got := HashKey(raw); got != hex.EncodeToString(want[:]) {
		t.Fatalf("HashKey mismatch: %s", got)
	}
}

func TestLookup(t *testing.T) {
	raw, hash, prefix, _ := GenerateKey()
	keys := []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}
	if _, ok := Lookup(keys, raw); !ok {
		t.Fatal("expected valid key to be found")
	}
	if _, ok := Lookup(keys, "ak-wrong"); ok {
		t.Fatal("expected invalid key to be rejected")
	}
}

func TestMiddlewareRejectsMissingKey(t *testing.T) {
	h := Middleware(func() *vault.Vault { return &vault.Vault{} })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/chat/completions", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestMiddlewareAllowsValidKey(t *testing.T) {
	raw, hash, prefix, _ := GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	h := Middleware(func() *vault.Vault { return v })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}
