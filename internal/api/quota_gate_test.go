package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/quota"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestQuotaEndpointsDisabledWhenConfigOff(t *testing.T) {
	off := false
	cfg := &config.Config{
		Dashboard: config.Dashboard{},
		Quota:     config.Quota{Enabled: &off},
	}
	cache := quota.NewCache()
	router := NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil, nil, "test", nil, cache)

	req := dashboardRequest("/internal/quota")
	req.Method = http.MethodGet
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("/internal/quota status = %d, want 404 when quota.enabled=false", rr.Code)
	}
}
