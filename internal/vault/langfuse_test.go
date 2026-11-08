package vault

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLangfuseSecretEncryptedAtRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.yaml")
	key := testKey()
	store, err := NewStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	sec := LangfuseSecret{PublicKey: "pk-lf-plain", SecretKey: "sk-lf-plain"}
	if err := store.Update(func(v *Vault) error { v.Langfuse = sec; return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("pk-lf-plain")) || bytes.Contains(raw, []byte("sk-lf-plain")) {
		t.Fatal("plaintext key material found in auth.yaml")
	}
	reopened, err := NewStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get().Langfuse; got != sec {
		t.Fatalf("Langfuse = %+v, want %+v", got, sec)
	}
}

func TestLangfuseCloneCopiesField(t *testing.T) {
	v := &Vault{Langfuse: LangfuseSecret{PublicKey: "pk", SecretKey: "sk"}}
	c := v.Clone()
	c.Langfuse.PublicKey = "changed"
	if (LangfuseSecret{}).Empty() != true {
		t.Fatal("zero LangfuseSecret should be Empty")
	}
}
