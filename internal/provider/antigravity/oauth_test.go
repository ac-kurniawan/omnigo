package antigravity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBuildAuthorizeURL(t *testing.T) {
	u, err := url.Parse(BuildAuthorizeURL("http://localhost:8080/oauth/callback", "state123"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" {
		t.Fatalf("response_type = %q", q.Get("response_type"))
	}
	if q.Get("access_type") != "offline" {
		t.Fatalf("access_type = %q", q.Get("access_type"))
	}
	if q.Get("client_id") == "" {
		t.Fatal("client_id empty")
	}
	if !strings.Contains(q.Get("scope"), "cloud-platform") {
		t.Fatalf("scope = %q", q.Get("scope"))
	}
}

func TestBuildAuthorizeURLDefaultRedirect(t *testing.T) {
	u, err := url.Parse(BuildAuthorizeURL("", "state123"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("redirect_uri") != DefaultRedirectURI {
		t.Fatalf("redirect_uri = %q, want %q", u.Query().Get("redirect_uri"), DefaultRedirectURI)
	}
}

func TestParseCallbackFullURL(t *testing.T) {
	rawURL := "http://127.0.0.1:20128/callback?state=W2KweerOVoDQ23npFQTpipygs2rV-BlSf1CMxsQ9iJg&iss=https://accounts.google.com&code=4/0ATsMZqDo7Yd9QqJHmKML4Hp5NoI8ho_8I5KjfglhRTyZqxnopqWivkV5oFUFKjTbfZPwlw&scope=email%20profile"
	code, redirectURI, err := ParseCallback(rawURL, DefaultRedirectURI)
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if code != "4/0ATsMZqDo7Yd9QqJHmKML4Hp5NoI8ho_8I5KjfglhRTyZqxnopqWivkV5oFUFKjTbfZPwlw" {
		t.Fatalf("code = %q", code)
	}
	if redirectURI != "http://127.0.0.1:20128/callback" {
		t.Fatalf("redirectURI = %q", redirectURI)
	}
}

func TestParseCallbackRawCode(t *testing.T) {
	rawCode := "4/0ATsMZqDo7Yd9QqJHmKML4Hp5NoI8ho_8I5KjfglhRTyZqxnopqWivkV5oFUFKjTbfZPwlw"
	code, redirectURI, err := ParseCallback(rawCode, DefaultRedirectURI)
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if code != rawCode {
		t.Fatalf("code = %q", code)
	}
	if redirectURI != DefaultRedirectURI {
		t.Fatalf("redirectURI = %q, want %q", redirectURI, DefaultRedirectURI)
	}
}

func TestParseCallbackErrorParam(t *testing.T) {
	errURL := "http://127.0.0.1:20128/callback?error=access_denied&error_description=User+cancelled"
	_, _, err := ParseCallback(errURL, DefaultRedirectURI)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "access_denied") || !strings.Contains(err.Error(), "User cancelled") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestExchangeCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" {
			t.Fatalf("grant_type = %q", r.Form.Get("grant_type"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at",
			"refresh_token": "rt",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()
	tokenURL = srv.URL + "/token"

	tok, err := ExchangeCode(context.Background(), "code", "http://localhost/cb")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" {
		t.Fatalf("token = %+v", tok)
	}
	if tok.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt not set")
	}
}

func TestRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Fatalf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("refresh_token") != "rt" {
			t.Fatalf("refresh_token = %q", r.Form.Get("refresh_token"))
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at2", "expires_in": 3600})
	}))
	defer srv.Close()
	tokenURL = srv.URL + "/token"

	tok, err := Refresh(context.Background(), "rt")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok.AccessToken != "at2" {
		t.Fatalf("token = %+v", tok)
	}
}
