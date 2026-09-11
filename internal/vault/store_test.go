package vault

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreUpdatePersistsAndSwaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.yaml")
	key := testKey()
	s, err := NewStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Get().ClientKeys) != 0 {
		t.Fatal("expected empty vault")
	}

	err = s.Update(func(v *Vault) error {
		v.ClientKeys = append(v.ClientKeys, ClientKey{ID: "k1", KeyHash: "abc"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(s.Get().ClientKeys) != 1 {
		t.Fatal("expected key after update")
	}

	got, err := Load(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ClientKeys) != 1 {
		t.Fatal("expected persisted key")
	}
}

func TestStoreGetSnapshotIsolatedFromUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.yaml")
	key := testKey()
	s, err := NewStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Update(func(v *Vault) error {
		v.ClientKeys = []ClientKey{{ID: "orig"}}
		return nil
	})
	snap := s.Get()
	_ = s.Update(func(v *Vault) error {
		v.ClientKeys = append(v.ClientKeys, ClientKey{ID: "added"})
		return nil
	})
	if len(snap.ClientKeys) != 1 {
		t.Fatalf("snapshot mutated: %+v", snap.ClientKeys)
	}
}

func TestStoreReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.yaml")
	key := testKey()
	s, err := NewStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Update(func(v *Vault) error { v.ClientKeys = []ClientKey{{ID: "k1"}}; return nil })

	v2 := &Vault{ClientKeys: []ClientKey{{ID: "k1"}, {ID: "k2"}}}
	if err := Save(path, key, v2); err != nil {
		t.Fatal(err)
	}

	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if len(s.Get().ClientKeys) != 2 {
		t.Fatalf("expected 2 keys after reload, got %d", len(s.Get().ClientKeys))
	}
}

func TestStoreNewOnMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.yaml")
	key := testKey()
	s, err := NewStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if s.Get() == nil {
		t.Fatal("expected non-nil vault")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("NewStore must not create the file")
	}
}
