package dashboard

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider/antigravity"
	"github.com/ac-kurniawan/omnigo/internal/provider/codex"
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

func TestOAuthLoginRedirectsToCodexWithPKCE(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex"}}}
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}}), nil)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest("GET", "/oauth/login/codex-main", nil))
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	u, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "auth.openai.com" || u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("prompt") != "login" {
		t.Fatalf("location = %s", u)
	}
	cookies := map[string]*http.Cookie{}
	for _, cookie := range rr.Result().Cookies() {
		cookies[cookie.Name] = cookie
	}
	verifier := cookies["omnigo_oauth_verifier"]
	if verifier == nil || verifier.Value == "" || !verifier.HttpOnly || verifier.SameSite != http.SameSiteLaxMode {
		t.Fatalf("verifier cookie = %+v", verifier)
	}
	if cookies["omnigo_oauth_state"] == nil || cookies["omnigo_oauth_state"].Value == "" || cookies["omnigo_oauth_provider"] == nil {
		t.Fatalf("cookies = %+v", cookies)
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
	s.discoverIdentity = func(r *http.Request, accessToken string) (antigravity.Identity, error) {
		return antigravity.Identity{AccountID: "google-1", Email: "user@example.com"}, nil
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

	sec := store.Get().Accounts("agy")[0]
	if sec.AccessToken != "at" || sec.RefreshToken != "rt" || sec.ProjectID != "proj-42" || sec.AccountID != "google-1" {
		t.Fatalf("secret = %+v", sec)
	}
}

func TestCodexOAuthCallbackPersistsIdentity(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	cfg := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)
	idToken := dashboardTestJWT(t, map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "workspace-1",
		},
	})
	var gotCode, gotVerifier, gotRedirectURI string
	s.codexExchange = func(r *http.Request, code, verifier, redirectURI string) (*codex.Token, error) {
		gotCode, gotVerifier, gotRedirectURI = code, verifier, redirectURI
		return &codex.Token{AccessToken: "at", RefreshToken: "rt", IDToken: idToken, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}

	req := httptest.NewRequest("GET", "/oauth/callback?state=st&code=c", nil)
	req.Host = "localhost:8080"
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_state", Value: "st"})
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_provider", Value: "codex-main"})
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_verifier", Value: "verifier"})
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if gotCode != "c" || gotVerifier != "verifier" || gotRedirectURI != codex.DefaultRedirectURI {
		t.Fatalf("exchange args = (%q, %q, %q)", gotCode, gotVerifier, gotRedirectURI)
	}
	sec := store.Get().Accounts("codex-main")[0]
	if sec.AccessToken != "at" || sec.RefreshToken != "rt" || sec.IDToken != idToken || sec.Email != "user@example.com" || sec.AccountID != "workspace-1" {
		t.Fatalf("secret = %+v", sec)
	}
	if strings.Contains(rr.Body.String(), "at") || strings.Contains(rr.Body.String(), "rt") || strings.Contains(rr.Body.String(), idToken) {
		t.Fatal("response exposed tokens")
	}
}

func TestCodexOAuthCallbacksAppendDistinctAccountsAndUpdateExisting(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	cfg := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)
	accounts := []struct {
		id      string
		email   string
		access  string
		refresh string
	}{
		{id: "workspace-1", email: "one@example.com", access: "access-1", refresh: "refresh-1"},
		{id: "workspace-2", email: "two@example.com", access: "access-2", refresh: "refresh-2"},
		{id: "workspace-1", email: "one@example.com", access: "access-1-new", refresh: "refresh-1-new"},
	}
	call := 0
	s.codexExchange = func(r *http.Request, code, verifier, redirectURI string) (*codex.Token, error) {
		account := accounts[call]
		call++
		return &codex.Token{
			AccessToken:  account.access,
			RefreshToken: account.refresh,
			IDToken: dashboardTestJWT(t, map[string]any{
				"email":                       account.email,
				"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account.id},
			}),
			ExpiresAt: time.Now().Add(time.Hour),
		}, nil
	}

	for range accounts {
		req := httptest.NewRequest("GET", "/oauth/callback?state=st&code=c", nil)
		req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "st"})
		req.AddCookie(&http.Cookie{Name: providerCookieName, Value: "codex-main"})
		req.AddCookie(&http.Cookie{Name: verifierCookieName, Value: "verifier"})
		rr := httptest.NewRecorder()
		s.routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
	}

	got := store.Get().ProviderAccounts["codex-main"]
	if len(got) != 2 {
		t.Fatalf("accounts = %+v", got)
	}
	if got[0].AccountID != "workspace-1" || got[0].AccessToken != "access-1-new" || got[0].Email != "one@example.com" {
		t.Fatalf("first account = %+v", got[0])
	}
	if got[1].AccountID != "workspace-2" || got[1].AccessToken != "access-2" {
		t.Fatalf("second account = %+v", got[1])
	}
}

func TestAntigravityOAuthCallbacksAppendAndUpdateByGoogleSubject(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	cfg := &config.Config{Providers: []config.Provider{{Name: "agy", Type: "antigravity"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)
	call := 0
	s.exchange = func(r *http.Request, code, redirectURI string) (*antigravity.Token, error) {
		call++
		return &antigravity.Token{AccessToken: "access-" + code, RefreshToken: "refresh-" + code, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	s.discover = func(r *http.Request, accessToken string) (string, error) { return "project", nil }
	s.discoverIdentity = func(r *http.Request, accessToken string) (antigravity.Identity, error) {
		if strings.Contains(accessToken, "second") {
			return antigravity.Identity{AccountID: "google-2", Email: "two@example.com"}, nil
		}
		return antigravity.Identity{AccountID: "google-1", Email: "one@example.com"}, nil
	}
	for _, code := range []string{"first", "second", "first-new"} {
		req := httptest.NewRequest("GET", "/oauth/callback?state=st&code="+code, nil)
		req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "st"})
		req.AddCookie(&http.Cookie{Name: providerCookieName, Value: "agy"})
		rr := httptest.NewRecorder()
		s.routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
	}
	accounts := store.Get().ProviderAccounts["agy"]
	if call != 3 || len(accounts) != 2 || accounts[0].AccountID != "google-1" || accounts[0].AccessToken != "access-first-new" || accounts[1].AccountID != "google-2" {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestCodexOAuthCallbackEscapesProviderName(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	const name = `<img src=x onerror=alert(1)>`
	cfg := &config.Config{Providers: []config.Provider{{Name: name, Type: "codex"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)
	idToken := dashboardTestJWT(t, map[string]any{
		"email":                       "user@example.com",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "workspace-xss"},
	})
	s.codexExchange = func(r *http.Request, code, verifier, redirectURI string) (*codex.Token, error) {
		return &codex.Token{AccessToken: "at", RefreshToken: "rt", IDToken: idToken}, nil
	}

	req := httptest.NewRequest("GET", "/oauth/callback?state=st&code=c", nil)
	req.Host = "localhost:8080"
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_state", Value: "st"})
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_provider", Value: name})
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_verifier", Value: "verifier"})
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "<img") {
		t.Fatalf("unescaped provider name in body: %s", body)
	}
	if !strings.Contains(body, "&lt;img") {
		t.Fatalf("escaped provider name missing from body: %s", body)
	}
}

func TestOAuthCallbackEscapesProviderName(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	const name = `<img src=x onerror=alert(1)>`
	cfg := &config.Config{Providers: []config.Provider{{Name: name, Type: "antigravity", BaseURL: "https://x"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)
	s.exchange = func(r *http.Request, code, redirectURI string) (*antigravity.Token, error) {
		return &antigravity.Token{AccessToken: "at", RefreshToken: "rt"}, nil
	}
	s.discover = func(r *http.Request, accessToken string) (string, error) {
		return "proj", nil
	}
	s.discoverIdentity = func(r *http.Request, accessToken string) (antigravity.Identity, error) {
		return antigravity.Identity{AccountID: "google-xss", Email: "user@example.com"}, nil
	}

	req := httptest.NewRequest("GET", "/oauth/callback?state=st&code=c", nil)
	req.Host = "localhost:8080"
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_state", Value: "st"})
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_provider", Value: name})
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "<img") {
		t.Fatalf("unescaped provider name in body: %s", body)
	}
	if !strings.Contains(body, "&lt;img") {
		t.Fatalf("escaped provider name missing from body: %s", body)
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
	s.discoverIdentity = func(r *http.Request, accessToken string) (antigravity.Identity, error) {
		return antigravity.Identity{AccountID: "google-pasted", Email: "pasted@example.com"}, nil
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

	sec := store.Get().Accounts("agy")[0]
	if sec.AccessToken != "at-paste" || sec.RefreshToken != "rt-paste" || sec.ProjectID != "proj-pasted" {
		t.Fatalf("stored secret = %+v", sec)
	}
}

func TestCodexOAuthPasteCallbackValidatesStateAndPersistsIdentity(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	cfg := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)
	idToken := dashboardTestJWT(t, map[string]any{
		"https://api.openai.com/profile": map[string]any{"email": "profile@example.com"},
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "workspace-2"},
	})
	s.codexExchange = func(r *http.Request, code, verifier, redirectURI string) (*codex.Token, error) {
		if code != "code-value" || verifier != "verifier" || redirectURI != codex.DefaultRedirectURI {
			t.Fatalf("exchange args = (%q, %q, %q)", code, verifier, redirectURI)
		}
		return &codex.Token{AccessToken: "access", RefreshToken: "refresh", IDToken: idToken}, nil
	}
	form := url.Values{"callback": {codex.DefaultRedirectURI + "?code=code-value&state=expected"}}
	req := httptest.NewRequest("POST", "/oauth/codex-main/paste-callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_state", Value: "expected"})
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_provider", Value: "codex-main"})
	req.AddCookie(&http.Cookie{Name: "omnigo_oauth_verifier", Value: "verifier"})
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	sec := store.Get().Accounts("codex-main")[0]
	if sec.Email != "profile@example.com" || sec.AccountID != "workspace-2" || sec.IDToken != idToken {
		t.Fatalf("secret = %+v", sec)
	}

	badReq := httptest.NewRequest("POST", "/oauth/codex-main/paste-callback", strings.NewReader(form.Encode()))
	badReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badReq.AddCookie(&http.Cookie{Name: "omnigo_oauth_state", Value: "wrong"})
	badReq.AddCookie(&http.Cookie{Name: "omnigo_oauth_provider", Value: "codex-main"})
	badReq.AddCookie(&http.Cookie{Name: "omnigo_oauth_verifier", Value: "verifier"})
	badRR := httptest.NewRecorder()
	s.routes().ServeHTTP(badRR, badReq)
	if badRR.Code != http.StatusBadRequest {
		t.Fatalf("state mismatch status = %d", badRR.Code)
	}
}

func dashboardTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
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
	s.discoverIdentity = func(r *http.Request, accessToken string) (antigravity.Identity, error) {
		return antigravity.Identity{AccountID: "google-code", Email: "code@example.com"}, nil
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
func TestCodexOAuthPasteCallbackCrossBrowser(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	cfg := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)

	// Step 1: Simulate Browser B requesting the login redirect
	loginReq := httptest.NewRequest("GET", "/oauth/login/codex-main", nil)
	loginRR := httptest.NewRecorder()
	s.routes().ServeHTTP(loginRR, loginReq)
	if loginRR.Code != http.StatusFound {
		t.Fatalf("login status = %d", loginRR.Code)
	}
	authLocation := loginRR.Header().Get("Location")
	authURL, err := url.Parse(authLocation)
	if err != nil {
		t.Fatalf("parse auth location: %v", err)
	}
	state := authURL.Query().Get("state")
	if state == "" {
		t.Fatal("auth redirect missing state")
	}

	// Step 2: Set up token exchange verification
	idToken := dashboardTestJWT(t, map[string]any{
		"email":                       "browser-b@example.com",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "workspace-shared"},
	})
	var capturedVerifier string
	s.codexExchange = func(r *http.Request, code, verifier, redirectURI string) (*codex.Token, error) {
		capturedVerifier = verifier
		return &codex.Token{
			AccessToken:  "access-b",
			RefreshToken: "refresh-b",
			IDToken:      idToken,
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}

	// Step 3: Simulate Browser A pasting the callback without Browser B's cookies
	callbackURL := codex.DefaultRedirectURI + "?code=code-from-browser-b&state=" + state
	form := url.Values{"callback": {callbackURL}}
	pasteReq := httptest.NewRequest("POST", "/oauth/codex-main/paste-callback", strings.NewReader(form.Encode()))
	pasteReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	pasteReq.Header.Set("HX-Request", "true")
	// Notice: pasteReq has NO cookies! Browser A was not the browser that initiated the flow.

	pasteRR := httptest.NewRecorder()
	s.routes().ServeHTTP(pasteRR, pasteReq)
	if pasteRR.Code != http.StatusOK {
		t.Fatalf("cross-browser paste status = %d, body = %s", pasteRR.Code, pasteRR.Body.String())
	}
	if capturedVerifier == "" {
		t.Fatal("expected non-empty PKCE verifier to be used in exchange")
	}

	accounts := store.Get().Accounts("codex-main")
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account stored, got %d", len(accounts))
	}
	if accounts[0].Email != "browser-b@example.com" {
		t.Fatalf("expected email browser-b@example.com, got %q", accounts[0].Email)
	}
}

func TestCodexOAuthCallbacksSameWorkspaceDistinctUsers(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	cfg := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)

	accountsData := []struct {
		email   string
		access  string
		refresh string
	}{
		{email: "alice@example.com", access: "access-alice", refresh: "refresh-alice"},
		{email: "bob@example.com", access: "access-bob", refresh: "refresh-bob"},
	}
	call := 0
	s.codexExchange = func(r *http.Request, code, verifier, redirectURI string) (*codex.Token, error) {
		data := accountsData[call]
		call++
		return &codex.Token{
			AccessToken:  data.access,
			RefreshToken: data.refresh,
			IDToken: dashboardTestJWT(t, map[string]any{
				"email":                       data.email,
				"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "shared-workspace"},
			}),
			ExpiresAt: time.Now().Add(time.Hour),
		}, nil
	}

	for _, acc := range accountsData {
		form := url.Values{"callback": {codex.DefaultRedirectURI + "?code=code-" + acc.email + "&state=st"}}
		req := httptest.NewRequest("POST", "/oauth/codex-main/paste-callback", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "st"})
		req.AddCookie(&http.Cookie{Name: providerCookieName, Value: "codex-main"})
		req.AddCookie(&http.Cookie{Name: verifierCookieName, Value: "test-verifier"})
		rr := httptest.NewRecorder()
		s.routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
	}

	got := store.Get().ProviderAccounts["codex-main"]
	if len(got) != 2 {
		t.Fatalf("expected 2 accounts for same workspace with different users, got %d: %+v", len(got), got)
	}
	if got[0].Email != "alice@example.com" || got[1].Email != "bob@example.com" {
		t.Fatalf("unexpected stored accounts: %+v", got)
	}
}
