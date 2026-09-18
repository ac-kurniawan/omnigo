package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/quota"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestGetQuotaReturnsAllSnapshots(t *testing.T) {
	cache := quota.NewCache()
	cache.Put(quota.AccountSnapshot{Provider: "cx", Identity: "a", Status: quota.StatusAvailable})
	h := handleGetQuota(cache)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/internal/quota", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out struct {
		Snapshots []quota.AccountSnapshot `json:"snapshots"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Snapshots) != 1 || out.Snapshots[0].Identity != "a" {
		t.Fatalf("snapshots = %+v", out.Snapshots)
	}
}

func TestGetQuotaNilCacheReturns404(t *testing.T) {
	h := handleGetQuota(nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/internal/quota", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestRefreshQuotaUpdatesCacheAndReturnsSnapshot(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{
		ProviderAccounts: map[string][]vault.ProviderSecret{
			"cx": {{AccountID: "a", Email: "a@x.com"}},
		},
	})
	reg := newProviderRegistry(store)
	cfg := &config.Config{Providers: []config.Provider{{Name: "cx", Type: "codex"}}}
	qp := &quotaTestProvider{snapshot: quota.AccountSnapshot{Status: quota.StatusAvailable}}
	reg.providers["cx"] = registryEntry{p: qp}
	reg.cfg = cfg

	cache := quota.NewCache()
	h := handleRefreshQuota(func() *config.Config { return cfg }, reg, cache)

	req := httptest.NewRequest(http.MethodPost, "/internal/refresh-quota/cx/a:a@x.com", nil)
	req.SetPathValue("provider", "cx")
	req.SetPathValue("identity", "a:a@x.com")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	snap, ok := cache.Get("cx", "a:a@x.com")
	if !ok || snap.Status != quota.StatusAvailable {
		t.Fatalf("cache = %+v", snap)
	}
}

func TestRouterRegistersQuotaEndpoints(t *testing.T) {
	cfg := &config.Config{
		Dashboard: config.Dashboard{},
		Providers: []config.Provider{{Name: "cx", Type: "codex"}},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	cache := quota.NewCache()
	cache.Put(quota.AccountSnapshot{Provider: "cx", Identity: "a", Status: quota.StatusAvailable})

	router := NewRouter(func() *config.Config { return cfg }, store, nil, nil, "test", nil, cache)

	req := dashboardRequest("/internal/quota")
	req.Method = http.MethodGet
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/internal/quota status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"provider":"cx"`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}
