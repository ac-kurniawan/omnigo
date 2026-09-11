package auth

import (
	"net/http"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func Middleware(getVault func() *vault.Vault) func(http.Handler) http.Handler {
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
			if _, ok := Lookup(v.ClientKeys, raw); !ok {
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
