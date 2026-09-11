package antigravity

import (
	"strings"
	"testing"
)

func TestUnmaskPublicCredentials(t *testing.T) {
	cid := getClientID()
	if !strings.HasSuffix(cid, ".apps.googleusercontent.com") {
		t.Fatalf("unexpected clientID: %s", cid)
	}
	sec := getClientSecret()
	if len(sec) < 20 {
		t.Fatalf("unexpected clientSecret length: %d", len(sec))
	}
}

func TestEnvOverrideCredentials(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "custom-client-id")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "custom-client-secret")

	if got := getClientID(); got != "custom-client-id" {
		t.Fatalf("getClientID = %q, want custom-client-id", got)
	}
	if got := getClientSecret(); got != "custom-client-secret" {
		t.Fatalf("getClientSecret = %q, want custom-client-secret", got)
	}
}
