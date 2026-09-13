package codex

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ClientID           = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultRedirectURI = "http://localhost:1455/auth/callback"
	authorizeURL       = "https://auth.openai.com/oauth/authorize"
	scopes             = "openid profile email offline_access"
)

var tokenURL = "https://auth.openai.com/oauth/token"

var httpClient = &http.Client{Timeout: 15 * time.Second}

type Token struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
}

func GeneratePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func BuildAuthorizeURL(redirectURI, state, codeChallenge string) string {
	if redirectURI == "" {
		redirectURI = DefaultRedirectURI
	}
	q := url.Values{
		"client_id":                  {ClientID},
		"response_type":              {"code"},
		"redirect_uri":               {redirectURI},
		"scope":                      {scopes},
		"state":                      {state},
		"code_challenge":             {codeChallenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"originator":                 {"codex_cli_rs"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return authorizeURL + "?" + q.Encode()
}

func ParseCallback(input, defaultRedirectURI string) (code, state, redirectURI string, err error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", "", fmt.Errorf("callback input is empty")
	}
	if defaultRedirectURI == "" {
		defaultRedirectURI = DefaultRedirectURI
	}
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		u, err := url.Parse(input)
		if err != nil {
			return "", "", "", fmt.Errorf("invalid callback URL: %w", err)
		}
		if err := callbackError(u.Query()); err != nil {
			return "", "", "", err
		}
		code = u.Query().Get("code")
		if code == "" {
			return "", "", "", fmt.Errorf("no authorization code found in callback URL")
		}
		return code, u.Query().Get("state"), u.Scheme + "://" + u.Host + u.Path, nil
	}
	if strings.Contains(input, "code=") || strings.Contains(input, "error=") {
		query := input
		if idx := strings.Index(query, "?"); idx >= 0 {
			query = query[idx+1:]
		}
		values, err := url.ParseQuery(query)
		if err != nil {
			return "", "", "", fmt.Errorf("invalid callback query: %w", err)
		}
		if err := callbackError(values); err != nil {
			return "", "", "", err
		}
		if values.Get("code") == "" {
			return "", "", "", fmt.Errorf("no authorization code found in callback query")
		}
		return values.Get("code"), values.Get("state"), defaultRedirectURI, nil
	}
	return input, "", defaultRedirectURI, nil
}

func callbackError(values url.Values) error {
	if values.Get("error") == "" {
		return nil
	}
	return fmt.Errorf("codex oauth authorization failed")
}

func ExchangeCode(ctx context.Context, code, verifier, redirectURI string) (*Token, error) {
	if redirectURI == "" {
		redirectURI = DefaultRedirectURI
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	return exchangeForm(ctx, form)
}

func exchangeForm(ctx context.Context, form url.Values) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return exchange(req)
}

func exchange(req *http.Request) (*Token, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token endpoint returned status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint returned no access token")
	}
	tok := &Token{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, IDToken: out.IDToken}
	if out.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	return tok, nil
}
