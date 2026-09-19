package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// Mobile browsers restore the dashboard from disk cache when the browser is
// reopened; without explicit cache headers they may never revalidate after an
// upgrade. Every dashboard response must therefore demand revalidation.
const wantCacheControl = "no-cache, must-revalidate"

func TestIndexSendsNoCacheHeader(t *testing.T) {
	cfg := &config.Config{}
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)

	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rr.Header().Get("Cache-Control"); got != wantCacheControl {
		t.Fatalf("GET / Cache-Control = %q, want %q", got, wantCacheControl)
	}
}

func TestStaticSendsNoCacheHeader(t *testing.T) {
	cfg := &config.Config{}
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)

	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/htmx.min.js", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /static/htmx.min.js status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != wantCacheControl {
		t.Fatalf("GET /static/htmx.min.js Cache-Control = %q, want %q", got, wantCacheControl)
	}
}
