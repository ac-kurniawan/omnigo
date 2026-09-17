package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
	"github.com/ac-kurniawan/omnigo/internal/version"
)

func TestIndexRendersBuildVersion(t *testing.T) {
	cfg := &config.Config{}
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)
	s.version = "v9.9.9"

	rr := httptest.NewRecorder()
	s.index(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()

	if !strings.Contains(body, ">v9.9.9<") {
		t.Fatalf("body missing build version badge: %s", body)
	}
	if strings.Contains(body, "v0.1 MVP") {
		t.Fatal("body still contains the hardcoded v0.1 MVP literal")
	}
}

func TestIndexRendersUnstampedDefaultVersion(t *testing.T) {
	cfg := &config.Config{}
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)

	rr := httptest.NewRecorder()
	s.index(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()

	if !strings.Contains(body, ">"+version.Value+"<") {
		t.Fatalf("body missing default version badge %q", version.Value)
	}
}
