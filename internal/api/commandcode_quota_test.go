package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/quota"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// A CommandCode account must reach the quota syncer and produce a cached
// snapshot the dashboard can render. This drives the real provider through the
// real registry rather than a stub, because the dashboard only shows a badge
// when a snapshot exists under the account's identity.
func TestCommandCodeQuotaReachesCache(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/alpha/billing/credits":
			_, _ = w.Write([]byte(`{"credits":{"monthlyCredits":9.5},"windowLimits":{"limited":true,
              "fiveHour":{"used":0.024,"cap":3,"exceeded":false,"resetAt":1794540754769},
              "weekly":{"used":0.4858,"cap":6,"exceeded":false,"resetAt":1795098681945}}}`))
		case "/alpha/usage/summary":
			_, _ = w.Write([]byte(`{"totalMonthlyCredits":0.5,"totalCredits":0.5,"periodStart":"2026-10-26T13:40:58.000Z","periodBasis":"billing-period"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	store := vault.NewMemoryStore(&vault.Vault{
		ProviderAccounts: map[string][]vault.ProviderSecret{
			"cmc": {{APIKey: "user_x"}},
		},
	})
	reg := newProviderRegistry(store)
	cfg := &config.Config{
		Providers: []config.Provider{{
			Name:    "cmc",
			Type:    "commandcode",
			BaseURL: ts.URL,
			Models:  []string{"zai-org/GLM-5.2"},
			Quota:   config.ProviderQuota{MonthlyCredits: 10, FiveHourCap: 3, WeeklyCap: 6},
		}},
	}
	p, err := buildProvider(cfg.Providers[0], store, reg.transport, config.Timeouts{})
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	reg.providers["cmc"] = registryEntry{typ: "commandcode", p: p}
	reg.cfg = cfg

	targets := reg.quotaTargets(func() *config.Config { return cfg })()
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want the commandcode account", targets)
	}
	if targets[0].Identity != "api-key" {
		t.Fatalf("identity = %q, want api-key", targets[0].Identity)
	}

	cache := quota.NewCache()
	snapshot, err := reg.quotaFetch(func() *config.Config { return cfg })(context.Background(), "cmc", targets[0].Identity)
	if err != nil {
		t.Fatalf("quotaFetch: %v", err)
	}
	if snapshot.Provider == "" {
		snapshot.Provider = "cmc"
	}
	cache.Put(snapshot)

	stored, ok := cache.Get("cmc", "api-key")
	if !ok {
		t.Fatal("snapshot did not reach the cache under (cmc, api-key)")
	}
	if len(stored.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(stored.Windows))
	}
	if stored.Windows[2].UsedPercent <= 0 {
		t.Fatalf("monthly UsedPercent = %v, want measured usage", stored.Windows[2].UsedPercent)
	}
	for i := 0; i < 2; i++ {
		if stored.Windows[i].UsedPercent <= 0 {
			t.Errorf("window %d UsedPercent = %v, want measured usage", i, stored.Windows[i].UsedPercent)
		}
	}
}
