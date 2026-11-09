package tracing

import (
	"os"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// Environment variable names for the Langfuse project key pair. They exist for
// deployments that cannot reach the dashboard (containers, CI), and take
// precedence over the pair stored in the vault when both halves are set.
const (
	EnvPublicKey = "LANGFUSE_PUBLIC_KEY"
	EnvSecretKey = "LANGFUSE_SECRET_KEY"
)

// ResolveKeys picks the key pair to export with, reading the environment on
// every call so a restarted container or an injected variable is picked up
// without code changes.
//
// Precedence: a complete pair in the environment wins, because an operator who
// set it means to override what the dashboard stored. A partial pair in the
// environment (one half empty) falls back to the vault rather than exporting
// with a half pair that would authenticate as garbage. With neither source
// complete the result is empty, which exports without credentials.
func ResolveKeys(env func(string) string, stored vault.LangfuseSecret) Keys {
	if env == nil {
		env = os.Getenv
	}
	publicKey := strings.TrimSpace(env(EnvPublicKey))
	secretKey := strings.TrimSpace(env(EnvSecretKey))
	if publicKey != "" && secretKey != "" {
		return Keys{PublicKey: publicKey, SecretKey: secretKey}
	}
	return Keys{PublicKey: stored.PublicKey, SecretKey: stored.SecretKey}
}
