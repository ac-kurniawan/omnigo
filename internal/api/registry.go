package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
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
}

type registryEntry struct {
	typ     string
	baseURL string
	timeout time.Duration
	p       provider.Provider
}

func newProviderRegistry(store *vault.Store) *providerRegistry {
	return &providerRegistry{
		store:     store,
		transport: newSharedTransport(),
		providers: make(map[string]registryEntry),
	}
}

func newSharedTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 100
	t.MaxIdleConnsPerHost = 20
	t.IdleConnTimeout = 90 * time.Second
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
	for _, pc := range cfg.Providers {
		timeout := pc.ParsedTimeout(cfg.DefaultTimeout())
		if prev, ok := r.providers[pc.Name]; ok && prev.typ == pc.Type && prev.baseURL == pc.BaseURL && prev.timeout == timeout {
			entries[pc.Name] = prev
			continue
		}
		p, err := buildProvider(pc, r.store, r.transport, cfg.DefaultTimeout())
		if err != nil {
			continue
		}
		entries[pc.Name] = registryEntry{typ: pc.Type, baseURL: pc.BaseURL, timeout: timeout, p: p}
	}
	r.providers = entries
	r.cfg = cfg
}
