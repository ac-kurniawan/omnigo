package vault

import (
	"os"
	"sync"
)

// Store is a concurrency-safe holder for the in-memory Vault with
// copy-on-write updates that persist to disk. Reads via Get return an
// immutable snapshot; writes go through Update (clone → mutate → save → swap).
type Store struct {
	mu   sync.RWMutex
	v    *Vault
	key  []byte
	path string
}

func NewStore(path string, key []byte) (*Store, error) {
	s := &Store{path: path, key: key}
	v := &Vault{ProviderSecrets: map[string]ProviderSecret{}}
	if _, err := os.Stat(path); err == nil {
		loaded, err := Load(path, key)
		if err != nil {
			return nil, err
		}
		v = loaded
	}
	s.v = v
	return s, nil
}

// NewMemoryStore returns a Store backed by v with no persistence (Update
// swaps in memory only). Useful for tests and embedding.
func NewMemoryStore(v *Vault) *Store {
	if v == nil {
		v = &Vault{ProviderSecrets: map[string]ProviderSecret{}}
	}
	return &Store{v: v}
}

// Get returns the current snapshot. The returned Vault is immutable (updates
// swap in a fresh copy), so concurrent reads are safe without holding a lock.
func (s *Store) Get() *Vault {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.v
}

// Update applies fn to a copy of the current Vault, persists it, and swaps it
// in. Concurrent updates are serialized; on error nothing is swapped or saved.
func (s *Store) Update(fn func(*Vault) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.v.Clone()
	if err := fn(cp); err != nil {
		return err
	}
	if s.path != "" {
		if err := Save(s.path, s.key, cp); err != nil {
			return err
		}
	}
	s.v = cp
	return nil
}

// Reload re-reads the file from disk and swaps it in.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := Load(s.path, s.key)
	if err != nil {
		return err
	}
	s.v = v
	return nil
}

// Clone returns a shallow-but-independent copy of the Vault (new map and
// slice backing arrays; element structs are value types).
func (v *Vault) Clone() *Vault {
	c := &Vault{
		ProviderSecrets: make(map[string]ProviderSecret, len(v.ProviderSecrets)),
		ClientKeys:      make([]ClientKey, len(v.ClientKeys)),
	}
	for k, val := range v.ProviderSecrets {
		c.ProviderSecrets[k] = val
	}
	copy(c.ClientKeys, v.ClientKeys)
	return c
}
