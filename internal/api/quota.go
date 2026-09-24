package api

import (
	"context"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/quota"
)

// quotaTargets enumerates the credentials the syncer should poll. It reads the
// live config and vault on every cycle, so adding an account or a provider
// through the dashboard takes effect on the next poll without a restart.
//
// Only providers that expose a quota endpoint are listed: a provider without
// one (an API-key openai provider, say) would otherwise contribute a permanent
// "unavailable" series.
func (r *providerRegistry) quotaTargets(cfg func() *config.Config) func() []quota.Target {
	return func() []quota.Target {
		current := cfg()
		if current == nil {
			return nil
		}
		var targets []quota.Target
		for _, pc := range current.Providers {
			if _, ok := provider.Get(pc.Type); !ok {
				continue
			}
			p, ok := r.Get(current, pc.Name)
			if !ok {
				continue
			}
			if _, ok := p.(provider.QuotaFetcher); !ok {
				continue
			}
			for _, account := range r.store.Get().Accounts(pc.Name) {
				creds := credentialsFromSecret(account)
				identity := creds.Identity()
				if identity == "" {
					continue
				}
				targets = append(targets, quota.Target{
					Provider: pc.Name,
					Identity: identity,
					Account:  accountLabel(creds),
				})
			}
		}
		return targets
	}
}

// accountLabel picks the metric label for a credential.
//
// Identity is used rather than AccountID because it is the only unique key: two
// logins into one ChatGPT workspace share an AccountID, and labelling by
// AccountID would merge their series so one account's exhaustion overwrites the
// other's availability. Identity never contains a credential.
func accountLabel(creds provider.Credentials) string {
	return creds.Identity()
}

// quotaFetch fetches one account's quota through the same provider instance
// that serves traffic.
//
// Reusing the instance matters: each Codex provider owns per-account token
// managers, and a second instance would run its own refresh cycle. Two
// independent refreshers racing on one rotating refresh token can invalidate
// the credential entirely, so polling and serving MUST share one instance.
func (r *providerRegistry) quotaFetch(cfg func() *config.Config) func(context.Context, string, string) (quota.AccountSnapshot, error) {
	return func(ctx context.Context, providerName, identity string) (quota.AccountSnapshot, error) {
		current := cfg()
		if current == nil {
			return quota.AccountSnapshot{}, errNoConfig
		}
		p, ok := r.Get(current, providerName)
		if !ok {
			return quota.AccountSnapshot{}, errProviderUnavailable
		}
		fetcher, ok := p.(provider.QuotaFetcher)
		if !ok {
			return quota.AccountSnapshot{}, errNoQuotaEndpoint
		}
		for _, account := range r.store.Get().Accounts(providerName) {
			creds := credentialsFromSecret(account)
			if creds.Identity() != identity {
				continue
			}
			return fetcher.FetchQuota(ctx, creds)
		}
		return quota.AccountSnapshot{}, errAccountGone
	}
}

// quotaDrainer routes a quota exhaustion into the named provider's account
// pool, so auto-drain removes exactly one credential from rotation while the
// provider and its combo targets stay routable through the other accounts.
func (r *providerRegistry) quotaDrainer(getCfg func() *config.Config) func(providerName, identity, model string, cooldown time.Duration, reason string) {
	return func(providerName, identity, model string, cooldown time.Duration, reason string) {
		cfg := getCfg()
		if cfg == nil {
			return
		}
		p, ok := r.Get(cfg, providerName)
		if !ok {
			return
		}
		pooled, ok := p.(interface {
			MarkQuotaDrained(identity, model string, cooldown time.Duration, reason string)
		})
		if !ok {
			return
		}
		pooled.MarkQuotaDrained(identity, model, cooldown, reason)
	}
}
