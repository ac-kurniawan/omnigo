package tracing

import (
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func envOf(vals map[string]string) func(string) string {
	return func(k string) string { return vals[k] }
}

func TestResolveKeysPrefersCompleteEnvPair(t *testing.T) {
	got := ResolveKeys(envOf(map[string]string{
		EnvPublicKey: "pk-env",
		EnvSecretKey: "sk-env",
	}), vault.LangfuseSecret{PublicKey: "pk-vault", SecretKey: "sk-vault"})
	if got != (Keys{PublicKey: "pk-env", SecretKey: "sk-env"}) {
		t.Fatalf("keys = %+v, want the env pair", got)
	}
}

func TestResolveKeysFallsBackToVault(t *testing.T) {
	stored := vault.LangfuseSecret{PublicKey: "pk-vault", SecretKey: "sk-vault"}
	if got := ResolveKeys(envOf(nil), stored); got != (Keys{PublicKey: "pk-vault", SecretKey: "sk-vault"}) {
		t.Fatalf("keys = %+v, want the vault pair", got)
	}
	// A half pair in the environment must not win: it would export with one
	// half of each source.
	for _, partial := range []map[string]string{
		{EnvPublicKey: "pk-env"},
		{EnvSecretKey: "sk-env"},
	} {
		if got := ResolveKeys(envOf(partial), stored); got != (Keys{PublicKey: "pk-vault", SecretKey: "sk-vault"}) {
			t.Fatalf("partial env %v -> %+v, want the vault pair", partial, got)
		}
	}
}

func TestResolveKeysTrimsWhitespace(t *testing.T) {
	got := ResolveKeys(envOf(map[string]string{
		EnvPublicKey: "  pk-env\n",
		EnvSecretKey: "\tsk-env ",
	}), vault.LangfuseSecret{})
	if got != (Keys{PublicKey: "pk-env", SecretKey: "sk-env"}) {
		t.Fatalf("keys = %+v", got)
	}
}

func TestResolveKeysEmptyWhenNeitherSourceHasAPair(t *testing.T) {
	if got := ResolveKeys(envOf(nil), vault.LangfuseSecret{}); got != (Keys{}) {
		t.Fatalf("keys = %+v, want empty", got)
	}
}

func TestResolveKeysUsesRealEnvironment(t *testing.T) {
	t.Setenv(EnvPublicKey, "pk-real")
	t.Setenv(EnvSecretKey, "sk-real")
	got := ResolveKeys(nil, vault.LangfuseSecret{PublicKey: "pk-vault", SecretKey: "sk-vault"})
	if got != (Keys{PublicKey: "pk-real", SecretKey: "sk-real"}) {
		t.Fatalf("keys = %+v, want the process environment pair", got)
	}
}
