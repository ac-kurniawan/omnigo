package dashboard

import (
	"crypto/subtle"
	"html"
	"net/http"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider/antigravity"
	"github.com/ac-kurniawan/omnigo/internal/provider/codex"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

const (
	stateCookieName    = "omnigo_oauth_state"
	providerCookieName = "omnigo_oauth_provider"
	verifierCookieName = "omnigo_oauth_verifier"
)

func (s *Server) findProvider(name string) *config.Provider {
	cfg := s.getCfg()
	for i := range cfg.Providers {
		if cfg.Providers[i].Name == name {
			return &cfg.Providers[i]
		}
	}
	return nil
}

func (s *Server) oauthLogin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	pc := s.findProvider(name)
	if pc == nil || (pc.Type != "antigravity" && pc.Type != "codex") {
		http.NotFound(w, r)
		return
	}

	state := randomHex(16)
	http.SetCookie(w, &http.Cookie{Name: stateCookieName, Value: state, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: providerCookieName, Value: name, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})

	if pc.Type == "codex" {
		verifier, challenge, err := codex.GeneratePKCE()
		if err != nil {
			http.Error(w, "failed to start oauth flow", http.StatusInternalServerError)
			return
		}
		s.recordOAuthPending(state, name, verifier)
		http.SetCookie(w, &http.Cookie{Name: verifierCookieName, Value: verifier, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, codex.BuildAuthorizeURL(codex.DefaultRedirectURI, state, challenge), http.StatusFound)
		return
	}

	s.recordOAuthPending(state, name, "")
	redirectURI := antigravity.DefaultRedirectURI
	http.Redirect(w, r, antigravity.BuildAuthorizeURL(redirectURI, state), http.StatusFound)
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	var name, verifier string
	if pendingProvider, pendingVerifier, ok := s.consumeOAuthPending(state); ok {
		name = pendingProvider
		verifier = pendingVerifier
	} else {
		stateCookie, _ := r.Cookie(stateCookieName)
		providerCookie, _ := r.Cookie(providerCookieName)
		if stateCookie == nil || providerCookie == nil || subtle.ConstantTimeCompare([]byte(state), []byte(stateCookie.Value)) != 1 {
			http.Error(w, "invalid oauth state", http.StatusBadRequest)
			return
		}
		name = providerCookie.Value
		if verifierCookie, _ := r.Cookie(verifierCookieName); verifierCookie != nil {
			verifier = verifierCookie.Value
		}
	}
	pc := s.findProvider(name)
	if pc == nil {
		http.Error(w, "unknown provider", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	redirectURI := scheme(r) + "://" + r.Host + "/oauth/callback"
	if pc.Type == "codex" {
		if verifier == "" {
			http.Error(w, "missing code verifier", http.StatusBadRequest)
			return
		}
		s.finishCodexLogin(w, r, name, code, verifier, codex.DefaultRedirectURI)
		return
	}

	tok, err := s.exchange(r, code, redirectURI)
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	projectID, err := s.discover(r, tok.AccessToken)
	if err != nil {
		projectID = ""
	}
	identity, err := s.discoverIdentity(r, tok.AccessToken)
	if err != nil || identity.AccountID == "" {
		http.Error(w, "failed to identify Google account", http.StatusBadGateway)
		return
	}

	err = s.store.Update(func(v *vault.Vault) error {
		v.UpsertAccount(name, vault.ProviderSecret{
			AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
			ExpiresAt: tok.ExpiresAt, ProjectID: projectID,
			AccountID: identity.AccountID, Email: identity.Email,
		})
		return nil
	})
	if err != nil {
		http.Error(w, "failed to persist credentials", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Connected</h1><p>Provider "` + html.EscapeString(name) + `" is now connected. <a href="/">Back to dashboard</a></p></body></html>`))
}

// finishCodexLogin exchanges a Codex authorization code, decodes the ID token
// identity, and persists the credentials.
func (s *Server) finishCodexLogin(w http.ResponseWriter, r *http.Request, name, code, verifier, redirectURI string) {
	tok, err := s.codexExchange(r, code, verifier, redirectURI)
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	claims, err := codex.ParseIDTokenClaims(tok.IDToken)
	if err != nil {
		http.Error(w, "invalid ID token", http.StatusBadGateway)
		return
	}

	if claims.UserID == "" && claims.AccountID == "" {
		http.Error(w, "Codex account ID is missing", http.StatusBadGateway)
		return
	}
	err = s.store.Update(func(v *vault.Vault) error {
		v.UpsertAccount(name, vault.ProviderSecret{
			AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
			IDToken: tok.IDToken, ExpiresAt: tok.ExpiresAt,
			Email: claims.Email, AccountID: claims.AccountID, UserID: claims.UserID,
		})
		return nil
	})
	if err != nil {
		http.Error(w, "failed to persist credentials", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") != "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Connected</h1><p>Provider "` + html.EscapeString(name) + `" is now connected. <a href="/">Back to dashboard</a></p></body></html>`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`
		<div class="alert alert-success shadow-md flex items-center justify-between my-2">
			<div class="flex items-center gap-2">
				<svg xmlns="http://www.w3.org/2000/svg" class="size-5 shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" /></svg>
				<span><strong>Codex Connected!</strong> Account authorized</span>
			</div>
			<button class="btn btn-ghost btn-xs" onclick="this.closest('.alert').remove()">✕</button>
		</div>
	`))
}

// oauthPasteCallback processes a user-pasted callback URL or raw code.
func (s *Server) oauthPasteCallback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	pc := s.findProvider(name)
	if pc == nil || (pc.Type != "antigravity" && pc.Type != "codex") {
		http.NotFound(w, r)
		return
	}

	rawInput := r.FormValue("callback")
	codeField := r.FormValue("code")

	if pc.Type == "codex" {
		s.codexPasteCallback(w, r, name, rawInput, codeField)
		return
	}

	var code, redirectURI string
	var err error

	// If unencoded URL was split by form parser, or "code" was directly posted:
	if codeField != "" && (rawInput == "" || !strings.Contains(rawInput, "code=")) {
		code = codeField
		if strings.HasPrefix(rawInput, "http://") || strings.HasPrefix(rawInput, "https://") {
			// Extract base redirect URI from scheme://host/path
			if idx := strings.Index(rawInput, "?"); idx != -1 {
				redirectURI = rawInput[:idx]
			} else {
				redirectURI = rawInput
			}
		}
		if redirectURI == "" {
			redirectURI = antigravity.DefaultRedirectURI
		}
	} else {
		code, redirectURI, err = antigravity.ParseCallback(rawInput, antigravity.DefaultRedirectURI)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	tok, err := s.exchange(r, code, redirectURI)
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	projectID, err := s.discover(r, tok.AccessToken)
	if err != nil {
		projectID = ""
	}
	identity, err := s.discoverIdentity(r, tok.AccessToken)
	if err != nil || identity.AccountID == "" {
		http.Error(w, "failed to identify Google account", http.StatusBadGateway)
		return
	}

	err = s.store.Update(func(v *vault.Vault) error {
		v.UpsertAccount(name, vault.ProviderSecret{
			AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
			ExpiresAt: tok.ExpiresAt, ProjectID: projectID,
			AccountID: identity.AccountID, Email: identity.Email,
		})
		return nil
	})
	if err != nil {
		http.Error(w, "failed to persist credentials", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`
		<div class="alert alert-success shadow-md flex items-center justify-between my-2">
			<div class="flex items-center gap-2">
				<svg xmlns="http://www.w3.org/2000/svg" class="size-5 shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" /></svg>
				<span><strong>Antigravity Connected!</strong> Account authorized (Project: ` + html.EscapeString(projectID) + `)</span>
			</div>
			<button class="btn btn-ghost btn-xs" onclick="this.closest('.alert').remove()">✕</button>
		</div>
	`))
}

// codexPasteCallback handles pasted Codex callbacks, validating state and
// using the PKCE verifier captured at login time.
func (s *Server) codexPasteCallback(w http.ResponseWriter, r *http.Request, name, rawInput, codeField string) {
	var code, state string
	var err error
	if codeField != "" && (rawInput == "" || !strings.Contains(rawInput, "code=")) {
		code = codeField
	} else {
		code, state, _, err = codex.ParseCallback(rawInput, codex.DefaultRedirectURI)
		if err != nil {
			http.Error(w, "invalid callback", http.StatusBadRequest)
			return
		}
	}
	var verifier string
	if state != "" {
		if pendingProvider, pendingVerifier, ok := s.consumeOAuthPending(state); ok {
			if pendingProvider != "" && pendingProvider != name {
				http.Error(w, "provider mismatch", http.StatusBadRequest)
				return
			}
			verifier = pendingVerifier
		}
	}

	if verifier == "" {
		stateCookie, _ := r.Cookie(stateCookieName)
		if state != "" {
			if stateCookie == nil || subtle.ConstantTimeCompare([]byte(state), []byte(stateCookie.Value)) != 1 {
				http.Error(w, "invalid oauth state", http.StatusBadRequest)
				return
			}
		}
		verifierCookie, _ := r.Cookie(verifierCookieName)
		if verifierCookie == nil || verifierCookie.Value == "" {
			http.Error(w, "missing code verifier", http.StatusBadRequest)
			return
		}
		verifier = verifierCookie.Value
	}

	s.finishCodexLogin(w, r, name, code, verifier, codex.DefaultRedirectURI)
}
func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
