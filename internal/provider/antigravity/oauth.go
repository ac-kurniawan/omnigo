package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	authorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"

	scopes = "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs"

	// DefaultRedirectURI is the Google desktop client loopback redirect URI
	// supported out-of-the-box by Antigravity's client ID.
	DefaultRedirectURI = "http://127.0.0.1:20128/callback"
)

// tokenURL is a var so tests can point it at an httptest server.
var tokenURL = "https://oauth2.googleapis.com/token"

type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

func BuildAuthorizeURL(redirectURI, state string) string {
	if redirectURI == "" {
		redirectURI = DefaultRedirectURI
	}
	q := url.Values{
		"client_id":     {getClientID()},
		"response_type": {"code"},
		"redirect_uri":  {redirectURI},
		"scope":         {scopes},
		"state":         {state},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
	}
	return authorizeURL + "?" + q.Encode()
}

// ParseCallback extracts code and redirectURI from user input.
// The input can be a full redirect URL, a relative path/query string, or a raw authorization code.
func ParseCallback(input, defaultRedirectURI string) (code, redirectURI string, err error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", fmt.Errorf("callback input is empty")
	}

	if defaultRedirectURI == "" {
		defaultRedirectURI = DefaultRedirectURI
	}

	// Case 1: Full URL (http://... or https://...)
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		u, err := url.Parse(input)
		if err != nil {
			return "", "", fmt.Errorf("invalid callback URL: %w", err)
		}
		if errParam := u.Query().Get("error"); errParam != "" {
			desc := u.Query().Get("error_description")
			if desc != "" {
				return "", "", fmt.Errorf("google oauth error: %s (%s)", errParam, desc)
			}
			return "", "", fmt.Errorf("google oauth error: %s", errParam)
		}
		code = u.Query().Get("code")
		if code == "" {
			return "", "", fmt.Errorf("no authorization code found in callback URL")
		}
		redirectURI = u.Scheme + "://" + u.Host + u.Path
		return code, redirectURI, nil
	}

	// Case 2: Query string or fragment (e.g. /callback?code=... or code=...)
	if strings.Contains(input, "code=") {
		queryStr := input
		if idx := strings.Index(input, "?"); idx != -1 {
			queryStr = input[idx+1:]
		}
		values, err := url.ParseQuery(queryStr)
		if err == nil && values.Get("code") != "" {
			if errParam := values.Get("error"); errParam != "" {
				return "", "", fmt.Errorf("google oauth error: %s", errParam)
			}
			return values.Get("code"), defaultRedirectURI, nil
		}
	}

	// Case 3: Raw authorization code directly (e.g. 4/0ATs...)
	return input, defaultRedirectURI, nil
}

func ExchangeCode(ctx context.Context, code, redirectURI string) (*Token, error) {
	if redirectURI == "" {
		redirectURI = DefaultRedirectURI
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {getClientID()},
		"client_secret": {getClientSecret()},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	return exchange(ctx, form)
}

func Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {getClientID()},
		"client_secret": {getClientSecret()},
		"refresh_token": {refreshToken},
	}
	return exchange(ctx, form)
}

func exchange(ctx context.Context, form url.Values) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := unaryClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange: status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	tok := &Token{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken}
	if out.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	return tok, nil
}
