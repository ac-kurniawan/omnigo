package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider/antigravity"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestOAuthLoginRedirectsToGoogle(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{{Name: "agy", Type: "antigravity", BaseURL: "https://x"}}}
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}}), nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/login/agy", nil)
	req.Host = "localhost:8080"
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "accounts.google.com/o/oauth2/v2/auth") {
		t.Fatalf("location = %s", loc)
	}
	if !strings.Contains(loc, "client_id=") || !strings.Contains(loc, "access_type=offline") {
		t.Fatalf("location missing params: %s", loc)
	}
	if rr.Result().Cookies() == nil || len(rr.Result().Cookies()) == 0 {
		t.Fatal("expected state cookie")
	}
}

func TestOAuthLoginRejectsNonAntigravity(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x"}}}
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest("GET", "/oauth/login/openai", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestOAuthCallbackPersistsTokens(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	cfg := &config.Config{Providers: []config.Provider{{Name: "agy", Type: "antigravity", BaseURL: "https://x"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)
	s.exchange = func(r *http.Request, code, redirectURI string) (*antigravity.Token, error) {
		return &antigravity.Token{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	s.discover = func(r *http.Request, accessToken string) (string, error) {
		return "proj-42", nil
	}

	req := httptest.NewRequest("GET", "/oauth/callback?state=st&code=c", nil)
	req.Host = "localhost:8080"
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_state", Value: "st"})
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_provider", Value: "agy"})

	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}

	sec := store.Get().ProviderSecrets["agy"]
	if sec.AccessToken != "at" || sec.RefreshToken != "rt" || sec.ProjectID != "proj-42" {
		t.Fatalf("secret = %+v", sec)
	}
}

func TestOAuthCallbackRejectsStateMismatch(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	cfg := &config.Config{}
	s := newServer(func() *config.Config { return cfg }, store, nil)
	req := httptest.NewRequest("GET", "/oauth/callback?state=other&code=c", nil)
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_state", Value: "st"})
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_provider", Value: "agy"})
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestOAuthPasteCallbackFullURL(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	cfg := &config.Config{Providers: []config.Provider{{Name: "agy", Type: "antigravity", BaseURL: "https://x"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)

	var gotCode, gotRedirectURI string
	s.exchange = func(r *http.Request, code, redirectURI string) (*antigravity.Token, error) {
		gotCode = code
		gotRedirectURI = redirectURI
		return &antigravity.Token{AccessToken: "at-paste", RefreshToken: "rt-paste", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	s.discover = func(r *http.Request, accessToken string) (string, error) {
		return "proj-pasted", nil
	}

	pastedURL := "http://127.0.0.1:20128/callback?state=W2KweerOVoDQ23npFQTpipygs2rV&code=4/0ATsMZqDo7Yd9QqJHmKML4Hp5NoI8ho&scope=email"
	req := httptest.NewRequest("POST", "/oauth/agy/paste-callback", strings.NewReader("callback="+pastedURL))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")

	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotCode != "4/0ATsMZqDo7Yd9QqJHmKML4Hp5NoI8ho" {
		t.Fatalf("gotCode = %q", gotCode)
	}
	if gotRedirectURI != "http://127.0.0.1:20128/callback" {
		t.Fatalf("gotRedirectURI = %q", gotRedirectURI)
	}

	sec := store.Get().ProviderSecrets["agy"]
	if sec.AccessToken != "at-paste" || sec.RefreshToken != "rt-paste" || sec.ProjectID != "proj-pasted" {
		t.Fatalf("stored secret = %+v", sec)
	}
}

func TestOAuthPasteCallbackRawCode(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	cfg := &config.Config{Providers: []config.Provider{{Name: "agy", Type: "antigravity", BaseURL: "https://x"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)

	var gotCode, gotRedirectURI string
	s.exchange = func(r *http.Request, code, redirectURI string) (*antigravity.Token, error) {
		gotCode = code
		gotRedirectURI = redirectURI
		return &antigravity.Token{AccessToken: "at-code", RefreshToken: "rt-code", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	s.discover = func(r *http.Request, accessToken string) (string, error) {
		return "proj-code", nil
	}

	rawCode := "4/0ATsMZqDo7Yd9QqJHmKML4Hp5NoI8ho"
	req := httptest.NewRequest("POST", "/oauth/agy/paste-callback", strings.NewReader("callback="+rawCode))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")

	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotCode != rawCode {
		t.Fatalf("gotCode = %q", gotCode)
	}
	if gotRedirectURI != antigravity.DefaultRedirectURI {
		t.Fatalf("gotRedirectURI = %q, want %q", gotRedirectURI, antigravity.DefaultRedirectURI)
	}
}
