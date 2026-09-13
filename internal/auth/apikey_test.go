package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
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
	keys := IndexKeys([]vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}})
	if _, ok := Lookup(keys, raw); !ok {
		t.Fatal("expected valid key to be found")
	}
	if _, ok := Lookup(keys, "ak-wrong"); ok {
		t.Fatal("expected invalid key to be rejected")
	}
}

func TestLookupConstantTimeComparisonRequiresFullHash(t *testing.T) {
	raw, hash, prefix, _ := GenerateKey()
	keys := IndexKeys([]vault.ClientKey{
		{ID: "short", KeyHash: hash[:len(hash)-1], Prefix: prefix, Active: true},
		{ID: "inactive", KeyHash: hash, Prefix: prefix, Active: false},
		{ID: "valid", KeyHash: hash, Prefix: prefix, Active: true},
	})

	got, ok := Lookup(keys, raw)
	if !ok || got.ID != "valid" {
		t.Fatalf("Lookup() = (%q, %v), want valid key", got.ID, ok)
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

func TestKeyIndexOmitsInactiveKeys(t *testing.T) {
	raw, hash, prefix, _ := GenerateKey()
	index := IndexKeys([]vault.ClientKey{
		{ID: "inactive", KeyHash: hash, Prefix: prefix, Active: false},
	})
	if _, ok := Lookup(index, raw); ok {
		t.Fatal("expected inactive key to be rejected")
	}
}

func TestMiddlewareRefreshesKeyIndex(t *testing.T) {
	raw1, hash1, prefix1, _ := GenerateKey()
	raw2, hash2, prefix2, _ := GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash1, Prefix: prefix1, Active: true}}}
	h := Middleware(func() *vault.Vault { return v })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	serve := func(raw string) int {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if got := serve(raw1); got != http.StatusOK {
		t.Fatalf("first key status = %d, want 200", got)
	}
	v = &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k2", KeyHash: hash2, Prefix: prefix2, Active: true}}}
	if got := serve(raw1); got != http.StatusUnauthorized {
		t.Fatalf("revoked key status = %d, want 401", got)
	}
	if got := serve(raw2); got != http.StatusOK {
		t.Fatalf("replacement key status = %d, want 200", got)
	}
}

func BenchmarkLookup(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("keys-%d", count), func(b *testing.B) {
			keys := make([]vault.ClientKey, count)
			var raw string
			for i := range keys {
				raw = fmt.Sprintf("ak-benchmark-%d", i)
				keys[i] = vault.ClientKey{ID: fmt.Sprint(i), KeyHash: HashKey(raw), Active: true}
			}
			index := IndexKeys(keys)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, ok := Lookup(index, raw); !ok {
					b.Fatal("key not found")
				}
			}
		})
	}
}

// BenchmarkLookupLinearScanBaseline reproduces the pre-change linear scan over
// the raw key slice so the O(1) index speedup is measurable in one run.
func BenchmarkLookupLinearScanBaseline(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("keys-%d", count), func(b *testing.B) {
			keys := make([]vault.ClientKey, count)
			var raw string
			for i := range keys {
				raw = fmt.Sprintf("ak-benchmark-%d", i)
				keys[i] = vault.ClientKey{ID: fmt.Sprint(i), KeyHash: HashKey(raw), Active: true}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				h := HashKey(raw)
				found := false
				for _, k := range keys {
					if k.Active && subtle.ConstantTimeCompare([]byte(k.KeyHash), []byte(h)) == 1 {
						found = true
						break
					}
				}
				if !found {
					b.Fatal("key not found")
				}
			}
		})
	}
}
