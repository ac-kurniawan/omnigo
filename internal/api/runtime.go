package api

import (
	"context"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/quota"
)

// ProviderRuntime exposes the provider registry to the process that owns it.
//
// The registry caches provider instances, and each Codex instance owns
// per-account token managers. Polling and serving therefore MUST share one
// registry: two instances would each run their own refresh cycle, and two
// refreshers racing on one rotating refresh token can invalidate the
// credential outright. Returning the runtime from NewRouterWithQuota lets the
// quota syncer and the dashboard reuse exactly the instances that serve
// traffic, instead of building their own.
type ProviderRuntime struct {
	registry *providerRegistry
	cache    *quota.Cache
}

// QuotaCache returns the cache the router publishes snapshots to.
func (rt *ProviderRuntime) QuotaCache() *quota.Cache {
	if rt == nil {
		return nil
	}
	return rt.cache
}

// QuotaTargets enumerates the credentials to poll on each syncer cycle.
func (rt *ProviderRuntime) QuotaTargets(getCfg func() *config.Config) func() []quota.Target {
	if rt == nil {
		return func() []quota.Target { return nil }
	}
	return rt.registry.quotaTargets(getCfg)
}

// QuotaFetch reads one credential's quota through the shared provider instance.
func (rt *ProviderRuntime) QuotaFetch(getCfg func() *config.Config) func(context.Context, string, string) (quota.AccountSnapshot, error) {
	if rt == nil {
		return func(context.Context, string, string) (quota.AccountSnapshot, error) {
			return quota.AccountSnapshot{}, errProviderUnavailable
		}
	}
	return rt.registry.quotaFetch(getCfg)
}

// QuotaDrainer returns a drainer that routes a quota exhaustion into the named
// provider's account pool.
func (rt *ProviderRuntime) QuotaDrainer(getCfg func() *config.Config) quota.Drainer {
	if rt == nil {
		return noopDrainer{}
	}
	return runtimeDrainer{drain: rt.registry.quotaDrainer(getCfg)}
}

// runtimeDrainer adapts the registry's drain function to quota.Drainer.
type runtimeDrainer struct {
	drain func(provider, identity, model string, cooldown time.Duration, reason string)
}

func (d runtimeDrainer) MarkQuotaDrained(provider, identity, model string, cooldown time.Duration, reason string) {
	d.drain(provider, identity, model, cooldown, reason)
}

// noopDrainer drops drains when no runtime is available.
type noopDrainer struct{}

func (noopDrainer) MarkQuotaDrained(string, string, string, time.Duration, string) {}

// RefreshQuota performs one synchronous poll and stores the result, backing
// the dashboard's per-account Refresh button.
func (rt *ProviderRuntime) RefreshQuota(ctx context.Context, getCfg func() *config.Config, providerName, identity string) (quota.AccountSnapshot, error) {
	if rt == nil {
		return quota.AccountSnapshot{}, errProviderUnavailable
	}
	snapshot, err := rt.registry.quotaFetch(getCfg)(ctx, providerName, identity)
	if snapshot.Provider == "" {
		snapshot.Provider = providerName
	}
	if snapshot.Identity == "" {
		snapshot.Identity = identity
	}
	if err != nil && snapshot.Status == "" {
		snapshot.Status = quota.StatusUnavailable
		snapshot.Reason = "quota fetch failed"
	}
	rt.cache.Put(snapshot)
	return snapshot, err
}
