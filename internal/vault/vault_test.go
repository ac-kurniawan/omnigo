package vault

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
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
	if got.Accounts("openai-main")[0].APIKey != "sk-secret" {
		t.Fatalf("round-trip lost secret: %+v", got.ProviderAccounts)
	}
	codex := got.Accounts("codex-main")[0]
	if codex.AccessToken != "access-secret" || codex.RefreshToken != "refresh-secret" || codex.IDToken != "id-secret" || codex.AccountID != "workspace-1" || codex.Email != "user@example.com" {
		t.Fatalf("round-trip lost Codex credentials: %+v", codex)
	}
	if len(got.ClientKeys) != 1 || got.ClientKeys[0].Prefix != "ak-12345678" {
		t.Fatalf("client keys = %+v", got.ClientKeys)
	}
}

func TestLoadLegacySingleAccountVault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.yaml")
	key := testKey()
	legacyPlaintext, err := yaml.Marshal(struct {
		ProviderSecrets map[string]ProviderSecret `yaml:"provider_secrets"`
	}{ProviderSecrets: map[string]ProviderSecret{
		"codex-main": {AccessToken: "access", RefreshToken: "refresh", AccountID: "workspace-1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := encrypt(key, legacyPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(file{Version: 1, Nonce: nonce, Ciphertext: ciphertext})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path, key)
	if err != nil {
		t.Fatal(err)
	}
	accounts := got.ProviderAccounts["codex-main"]
	if len(accounts) != 1 || accounts[0].AccountID != "workspace-1" || accounts[0].AccessToken != "access" {
		t.Fatalf("vault = %+v", got)
	}
	migrated, err := Load(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrated.ProviderSecrets) != 0 || len(migrated.ProviderAccounts["codex-main"]) != 1 {
		t.Fatalf("migrated vault = %+v", migrated)
	}
}

func TestRoundTripProviderAccountsEncrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.yaml")
	key := testKey()
	v := &Vault{ProviderAccounts: map[string][]ProviderSecret{
		"codex-main": {
			{AccessToken: "access-1", RefreshToken: "refresh-1", AccountID: "account-1"},
			{AccessToken: "access-2", RefreshToken: "refresh-2", AccountID: "account-2"},
		},
	}}
	if err := Save(path, key, v); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("access-1")) || bytes.Contains(raw, []byte("refresh-2")) {
		t.Fatal("vault persisted plaintext credentials")
	}
	got, err := Load(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ProviderAccounts["codex-main"]) != 2 || got.ProviderAccounts["codex-main"][1].AccountID != "account-2" {
		t.Fatalf("accounts = %+v", got.ProviderAccounts)
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
func TestVaultUpsertSameAccountIDDifferentEmail(t *testing.T) {
	v := &Vault{ProviderAccounts: map[string][]ProviderSecret{}}
	v.UpsertAccount("codex", ProviderSecret{AccountID: "ws-1", Email: "alice@example.com", AccessToken: "tok-alice"})
	v.UpsertAccount("codex", ProviderSecret{AccountID: "ws-1", Email: "bob@example.com", AccessToken: "tok-bob"})

	accounts := v.Accounts("codex")
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts for distinct emails in same workspace, got %d", len(accounts))
	}

	v.UpsertAccount("codex", ProviderSecret{AccountID: "ws-1", Email: "alice@example.com", AccessToken: "tok-alice-v2"})
	accounts = v.Accounts("codex")
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts after update, got %d", len(accounts))
	}
	if accounts[0].AccessToken != "tok-alice-v2" {
		t.Fatalf("expected alice updated to tok-alice-v2, got %q", accounts[0].AccessToken)
	}
	if accounts[1].AccessToken != "tok-bob" {
		t.Fatalf("expected bob untouched with tok-bob, got %q", accounts[1].AccessToken)
	}
}
