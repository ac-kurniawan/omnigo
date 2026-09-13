package vault

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func testKey() []byte { return bytes.Repeat([]byte{0xAB}, 32) }

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.yaml")
	key := testKey()
	v := &Vault{
		ProviderSecrets: map[string]ProviderSecret{
			"openai-main": {APIKey: "sk-secret"},
			"codex-main": {
				AccessToken:  "access-secret",
				RefreshToken: "refresh-secret",
				IDToken:      "id-secret",
				AccountID:    "workspace-1",
				Email:        "user@example.com",
			},
		},
		ClientKeys: []ClientKey{{ID: "k1", Name: "dev", KeyHash: "abc", Prefix: "ak-12345678"}},
	}
	if err := Save(path, key, v); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ProviderSecrets["openai-main"].APIKey != "sk-secret" {
		t.Fatalf("round-trip lost secret: %+v", got.ProviderSecrets)
	}
	codex := got.ProviderSecrets["codex-main"]
	if codex.AccessToken != "access-secret" || codex.RefreshToken != "refresh-secret" || codex.IDToken != "id-secret" || codex.AccountID != "workspace-1" || codex.Email != "user@example.com" {
		t.Fatalf("round-trip lost Codex credentials: %+v", codex)
	}
	if len(got.ClientKeys) != 1 || got.ClientKeys[0].Prefix != "ak-12345678" {
		t.Fatalf("client keys = %+v", got.ClientKeys)
	}
}

func TestLoadWrongKeyFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.yaml")
	key := testKey()
	if err := Save(path, key, &Vault{}); err != nil {
		t.Fatal(err)
	}
	wrong := bytes.Repeat([]byte{0xCD}, 32)
	if _, err := Load(path, wrong); err == nil {
		t.Fatal("expected error with wrong key")
	}
}

func TestResolveKeyFromEnv(t *testing.T) {
	env := "0123456789abcdef0123456789abcdef" // 32 bytes
	key, err := ResolveKey(env, filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key length = %d, want 32", len(key))
	}
}

func TestResolveKeyGeneratesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".secret.key")
	key, err := ResolveKey("", path)
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key length = %d, want 32", len(key))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("key file not created: %v", err)
	}
}
