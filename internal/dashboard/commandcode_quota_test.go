package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/quota"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// The dashboard renders a quota badge per account. CommandCode stores only an
// API key, whose identity is the literal "api-key", so the cache key, the
// template lookup, and the rendered rows all have to agree on that value.
func TestDashboardRendersCommandCodeQuota(t *testing.T) {
	disabled := false
	cfg := &config.Config{
		Dashboard: config.Dashboard{Auth: &disabled},
		Providers: []config.Provider{{
			Name:   "cmc",
			Type:   "commandcode",
			Models: []string{"zai-org/GLM-5.2"},
		}},
	}
	v := &vault.Vault{ProviderAccounts: map[string][]vault.ProviderSecret{
		"cmc": {{APIKey: "user_x"}},
	}}

	cache := quota.NewCache()
	cache.Put(quota.AccountSnapshot{
		Provider:   "cmc",
		Identity:   "api-key",
		Status:     quota.StatusAvailable,
		ObservedAt: time.Now(),
		Windows: []quota.Window{
			{Name: "5-hour", Display: "cap 3 credits", WindowMinutes: 300},
			{Name: "weekly", Display: "cap 6 credits", WindowMinutes: 10080},
			{Name: "monthly", UsedPercent: 4.93, WindowMinutes: 43200},
		},
	})

	h := NewHandlerWithQuota(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, cache)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{"5-hour limit", "Weekly limit", "Monthly limit"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "Not checked") {
		t.Error("account rendered as 'Not checked' despite a cached snapshot")
	}
}
