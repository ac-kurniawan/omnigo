package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/quota"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// providerRegistry caches provider instances and the shared HTTP transport
// across requests, rebuilding entries when the config snapshot changes.
type providerRegistry struct {
	store     *vault.Store
	transport *http.Transport

	mu        sync.RWMutex
	cfg       *config.Config
	providers map[string]registryEntry

	// quotaCache receives snapshots observed on live inference responses.
	// It is optional; when nil the observer is not installed.
	quotaCache *quota.Cache
}

type registryEntry struct {
	typ            string
	baseURL        string
	timeout        time.Duration
	streamTimeout  time.Duration
	maxConcurrency int
	p              provider.Provider
}

func newProviderRegistry(store *vault.Store) *providerRegistry {
	return &providerRegistry{
		store:     store,
		transport: newSharedTransport(),
		providers: make(map[string]registryEntry),
	}
}

// newSharedTransport builds the connection pool shared by every provider.
// ResponseHeaderTimeout stays unset: a fixed cap here would undercut a provider
// configured with a longer timeout, and time to first byte is already bounded
// per request by the buffered client's timeout and by the stream idle guard.
func newSharedTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 100
	t.MaxIdleConnsPerHost = 100
	t.IdleConnTimeout = 90 * time.Second
	t.ForceAttemptHTTP2 = true
	return t
}

func (r *providerRegistry) Get(cfg *config.Config, name string) (provider.Provider, bool) {
	r.ensure(cfg)
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.providers[name]
	if !ok {
		return nil, false
	}
	return entry.p, true
}

// ensure rebuilds the registry when cfg is a new snapshot. Providers whose
// type, base URL, and timeout are unchanged are reused.
func (r *providerRegistry) ensure(cfg *config.Config) {
	r.mu.RLock()
	loaded := r.cfg
	r.mu.RUnlock()
	if loaded == cfg {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg == cfg {
		return
	}
	entries := make(map[string]registryEntry, len(cfg.Providers))
	timeouts := cfg.Timeouts()
	for _, pc := range cfg.Providers {
		timeout := pc.ParsedTimeout(timeouts.Request)
		streamTimeout := pc.ParsedStreamTimeout(timeouts.Stream)
		if prev, ok := r.providers[pc.Name]; ok && prev.typ == pc.Type && prev.baseURL == pc.BaseURL && prev.timeout == timeout && prev.streamTimeout == streamTimeout && prev.maxConcurrency == pc.MaxConcurrency {
			entries[pc.Name] = prev
			continue
		}
		p, err := buildProvider(pc, r.store, r.transport, timeouts)
		if err != nil {
			continue
		}
		r.observeQuota(p)
		entries[pc.Name] = registryEntry{typ: pc.Type, baseURL: pc.BaseURL, timeout: timeout, streamTimeout: streamTimeout, maxConcurrency: pc.MaxConcurrency, p: p}
	}
	r.providers = entries
	r.cfg = cfg
}

// observeQuota installs the quota cache as the provider's snapshot sink, so a
// snapshot observed on an ordinary inference response reaches the dashboard
// and the registry without waiting for the next poll.
//
// The observer is push-based and holds no reference to the registry, so a
// provider instance that survives a config reload keeps feeding the same
// cache.
func (r *providerRegistry) observeQuota(p provider.Provider) {
	if r.quotaCache == nil {
		return
	}
	observer, ok := p.(interface {
		SetQuotaObserver(func(quota.AccountSnapshot))
	})
	if !ok {
		return
	}
	observer.SetQuotaObserver(r.quotaCache.Put)
}
