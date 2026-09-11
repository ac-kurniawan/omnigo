package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestCreateKeyReturnsRawOnce(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	cfg := &config.Config{}
	s := newServer(func() *config.Config { return cfg }, store, nil)

	req := httptest.NewRequest("POST", "/keys", strings.NewReader("name=dev"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "ak-") {
		t.Fatalf("body = %s", body)
	}

	keys := store.Get().ClientKeys
	if len(keys) != 1 || !keys[0].Active || keys[0].Name != "dev" {
		t.Fatalf("keys = %+v", keys)
	}
}

func TestRevokeKeyDeactivates(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{
		ProviderSecrets: map[string]vault.ProviderSecret{},
		ClientKeys:      []vault.ClientKey{{ID: "k1", Name: "dev", Prefix: "ak-12345678", Active: true}},
	})
	cfg := &config.Config{}
	s := newServer(func() *config.Config { return cfg }, store, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/keys/k1/revoke", nil)
	req.Header.Set("HX-Request", "true")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	keys := store.Get().ClientKeys
	if len(keys) != 1 || keys[0].Active {
		t.Fatalf("expected revoked key, got %+v", keys)
	}

	body := rr.Body.String()
	if strings.Contains(body, "ak-12345678") {
		t.Fatalf("revoked key prefix should be removed from dashboard, got %s", body)
	}
	if strings.Contains(body, "revoked") {
		t.Fatalf("revoked status badge should be removed from dashboard, got %s", body)
	}
}

func TestIndexDoesNotRenderRevokedKey(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{
		ProviderSecrets: map[string]vault.ProviderSecret{},
		ClientKeys: []vault.ClientKey{
			{ID: "k1", Name: "dev-revoked", Prefix: "ak-11111111", Active: false},
			{ID: "k2", Name: "dev-active", Prefix: "ak-22222222", Active: true},
		},
	})
	cfg := &config.Config{}
	s := newServer(func() *config.Config { return cfg }, store, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	body := rr.Body.String()
	if strings.Contains(body, "ak-11111111") {
		t.Fatalf("revoked key should not be rendered on index dashboard")
	}
	if !strings.Contains(body, "ak-22222222") {
		t.Fatalf("active key should be rendered on index dashboard")
	}
}
