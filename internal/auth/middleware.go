package auth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
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

func DashboardMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("omnigo_csrf")
		if err != nil || r.Header.Get("HX-Request") != "true" || !sameOrigin(r) || !sameToken(cookie.Value, r.Header.Get("X-Omnigo-CSRF")) {
			dashboardUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	source := r.Header.Get("Origin")
	if source == "" {
		source = r.Header.Get("Referer")
	}
	if source == "" {
		return false
	}
	u, err := url.Parse(source)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host) && (u.Scheme == "http" || u.Scheme == "https")
}

func sameToken(cookie, header string) bool {
	return cookie != "" && header != "" && subtle.ConstantTimeCompare([]byte(cookie), []byte(header)) == 1
}

func dashboardUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": "dashboard authorization failed; reload the dashboard and try again", "type": "invalid_request_error", "code": "dashboard_authorization_failed"},
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"message":"invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`))
}
