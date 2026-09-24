package api

import (
	"context"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/quota"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

type quotaTestProvider struct {
	fakeProvider
	snapshot quota.AccountSnapshot
	err      error
	drains   map[string]time.Duration
}

func (q *quotaTestProvider) FetchQuota(ctx context.Context, account provider.Credentials) (quota.AccountSnapshot, error) {
	return q.snapshot, q.err
}

func (q *quotaTestProvider) MarkQuotaDrained(identity, model string, cooldown time.Duration, reason string) {
	if q.drains == nil {
		q.drains = make(map[string]time.Duration)
	}
	q.drains[identity] = cooldown
}

func TestQuotaTargetsListsOnlyQuotaCapableProviders(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{
		ProviderAccounts: map[string][]vault.ProviderSecret{
			"with-quota":    {{AccountID: "one", Email: "one@example.com"}},
			"without-quota": {{AccountID: "two"}},
		},
	})
	reg := newProviderRegistry(store)
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "with-quota", Type: "quota-type"},
			{Name: "without-quota", Type: "plain-type"},
		},
	}
	reg.providers["with-quota"] = registryEntry{p: &quotaTestProvider{}}
	reg.providers["without-quota"] = registryEntry{p: &fakeProvider{}}
	reg.cfg = cfg

	// Register the test type so provider.Get recognizes it.
	provider.Register("quota-type", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &quotaTestProvider{}
	})
	provider.Register("plain-type", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{}
	})

	targets := reg.quotaTargets(func() *config.Config { return cfg })()
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want only the quota-capable provider", targets)
	}
	if targets[0].Provider != "with-quota" || targets[0].Account != "one:one@example.com" {
		t.Fatalf("target = %+v", targets[0])
	}
}

func TestQuotaFetchReusesCachedProviderInstance(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{
		ProviderAccounts: map[string][]vault.ProviderSecret{
			"cx": {{AccountID: "acct-1", Email: "a@b.com"}},
		},
	})
	reg := newProviderRegistry(store)
	cfg := &config.Config{Providers: []config.Provider{{Name: "cx", Type: "codex"}}}
	qp := &quotaTestProvider{snapshot: quota.AccountSnapshot{Status: quota.StatusAvailable}}
	reg.providers["cx"] = registryEntry{p: qp}
	reg.cfg = cfg

	fetch := reg.quotaFetch(func() *config.Config { return cfg })
	snap, err := fetch(context.Background(), "cx", "acct-1:a@b.com")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != quota.StatusAvailable {
		t.Fatalf("status = %s", snap.Status)
	}
}

func TestQuotaDrainerCoolsAccount(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{})
	reg := newProviderRegistry(store)
	cfg := &config.Config{Providers: []config.Provider{{Name: "cx", Type: "codex"}}}
	qp := &quotaTestProvider{}
	reg.providers["cx"] = registryEntry{p: qp}
	reg.cfg = cfg

	drain := reg.quotaDrainer(func() *config.Config { return cfg })
	drain("cx", "acct-1", "model-a", 10*time.Minute, "quota")

	if got := qp.drains["acct-1"]; got != 10*time.Minute {
		t.Fatalf("drains = %+v, want 10m on acct-1", qp.drains)
	}
}

// Two logins into one ChatGPT workspace share an AccountID. The metric label
// must follow Identity, or their series merge and one account's exhaustion
// silently overwrites the other's availability.
func TestAccountLabelKeepsSharedAccountIDDistinct(t *testing.T) {
	workspace := "fb88ebc3-79ae-45ff-b8b1-2313efa099b4"
	first := provider.Credentials{AccountID: workspace, Email: "20110460+networkhr@utc2eduvn.onmicrosoft.com"}
	second := provider.Credentials{AccountID: workspace, UserID: "user-XTokGp1fOniGFSYxMyTub64g", Email: "10647509+charlieflight@utc2eduvn.onmicrosoft.com"}

	a, b := accountLabel(first), accountLabel(second)
	if a == b {
		t.Fatalf("distinct accounts in one workspace collapsed to %q", a)
	}
	if a != first.Identity() || b != second.Identity() {
		t.Fatalf("labels must be identities: got %q / %q", a, b)
	}
}
