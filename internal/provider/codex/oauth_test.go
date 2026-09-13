package codex

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGeneratePKCE(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("verifier length = %d", len(verifier))
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Fatalf("challenge = %q, want %q", challenge, want)
	}
}

func TestBuildAuthorizeURL(t *testing.T) {
	u, err := url.Parse(BuildAuthorizeURL("", "state-value", "challenge-value"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "auth.openai.com" || u.Path != "/oauth/authorize" {
		t.Fatalf("authorize URL = %s", u)
	}
	want := map[string]string{
		"client_id":                  ClientID,
		"response_type":              "code",
		"redirect_uri":               DefaultRedirectURI,
		"scope":                      "openid profile email offline_access",
		"state":                      "state-value",
		"code_challenge":             "challenge-value",
		"code_challenge_method":      "S256",
		"prompt":                     "login",
		"originator":                 "codex_cli_rs",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
	}
	for key, value := range want {
		if got := u.Query().Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

func TestParseCallback(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantCode  string
		wantState string
		wantURI   string
		wantErr   bool
	}{
		{"full URL", "http://localhost:1455/auth/callback?code=abc&state=xyz", "abc", "xyz", "http://localhost:1455/auth/callback", false},
		{"query", "/auth/callback?code=abc&state=xyz", "abc", "xyz", DefaultRedirectURI, false},
		{"raw code", "abc", "abc", "", DefaultRedirectURI, false},
		{"oauth error", "http://localhost:1455/auth/callback?error=access_denied&error_description=no", "", "", "", true},
		{"missing code", "http://localhost:1455/auth/callback?state=xyz", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, state, redirectURI, err := ParseCallback(tt.input, "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if code != tt.wantCode || state != tt.wantState || redirectURI != tt.wantURI {
				t.Fatalf("got (%q, %q, %q)", code, state, redirectURI)
			}
		})
	}
}

func TestExchangeCode(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"grant_type":    "authorization_code",
			"client_id":     ClientID,
			"code":          "auth-code",
			"redirect_uri":  "http://localhost/callback",
			"code_verifier": "verifier",
		}
		for key, value := range want {
			if r.Form.Get(key) != value {
				t.Errorf("%s = %q, want %q", key, r.Form.Get(key), value)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access",
			"refresh_token": "refresh",
			"id_token":      "id",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	tokenURL = server.URL

	before := time.Now().Add(59 * time.Minute)
	tok, err := ExchangeCode(context.Background(), "auth-code", "verifier", "http://localhost/callback")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access" || tok.RefreshToken != "refresh" || tok.IDToken != "id" || tok.ExpiresAt.Before(before) {
		t.Fatalf("token = %+v", tok)
	}
}

func TestExchangeCodeSanitizesUpstreamError(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"secret-code"}`))
	}))
	defer server.Close()
	tokenURL = server.URL

	_, err := ExchangeCode(context.Background(), "secret-code", "secret-verifier", "")
	if err == nil || strings.Contains(err.Error(), "secret-code") || strings.Contains(err.Error(), "secret-verifier") {
		t.Fatalf("error = %v", err)
	}
}
