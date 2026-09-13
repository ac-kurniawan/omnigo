package auth

import (
	"net/http"
	"strings"
	"sync"

	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func Middleware(getVault func() *vault.Vault) func(http.Handler) http.Handler {
	var mu sync.RWMutex
	var cachedVault *vault.Vault
	var keys map[string]vault.ClientKey
	lookup := func(v *vault.Vault, raw string) bool {
		mu.RLock()
		if v == cachedVault {
			_, ok := Lookup(keys, raw)
			mu.RUnlock()
			return ok
		}
		mu.RUnlock()

		mu.Lock()
		if v != cachedVault {
			keys = IndexKeys(v.ClientKeys)
			cachedVault = v
		}
		_, ok := Lookup(keys, raw)
		mu.Unlock()
		return ok
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if raw == "" || raw == r.Header.Get("Authorization") {
				unauthorized(w)
				return
			}
			v := getVault()
			if v == nil {
				unauthorized(w)
				return
			}
			if !lookup(v, raw) {
				unauthorized(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"message":"invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`))
}
