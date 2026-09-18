package quota

import "sync"

// Cache is the in-memory quota snapshot store. Snapshots live only for the
// process lifetime: they are derived data that the syncer rebuilds on every
// poll, so persisting them would add a file holding account-identifying
// information for no benefit.
//
// The zero value is not usable; construct with NewCache.
type Cache struct {
	mu        sync.RWMutex
	snapshots map[string]AccountSnapshot
}

func NewCache() *Cache {
	return &Cache{snapshots: make(map[string]AccountSnapshot)}
}

// cacheKey qualifies the credential identity with the provider name. The same
// identity string can legitimately appear under two providers, and each has
// its own quota state.
func cacheKey(provider, identity string) string {
	return provider + "/" + identity
}

// Put stores a snapshot, replacing any prior state for the same credential.
// An empty provider or identity is ignored: without both, a snapshot cannot
// be attributed to an account and would be unreachable through Get.
func (c *Cache) Put(snapshot AccountSnapshot) {
	if snapshot.Provider == "" || snapshot.Identity == "" {
		return
	}
	c.mu.Lock()
	c.snapshots[cacheKey(snapshot.Provider, snapshot.Identity)] = snapshot
	c.mu.Unlock()
}

// Get returns the snapshot for one credential.
func (c *Cache) Get(provider, identity string) (AccountSnapshot, bool) {
	c.mu.RLock()
	snapshot, ok := c.snapshots[cacheKey(provider, identity)]
	c.mu.RUnlock()
	return snapshot, ok
}

// All returns every snapshot. The returned slice is a copy, so callers may
// sort or mutate it without disturbing the cache.
func (c *Cache) All() []AccountSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]AccountSnapshot, 0, len(c.snapshots))
	for _, snapshot := range c.snapshots {
		out = append(out, snapshot)
	}
	return out
}
