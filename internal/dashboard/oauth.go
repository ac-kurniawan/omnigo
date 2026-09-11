package dashboard

import (
	"net/http"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider/antigravity"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func (s *Server) oauthLogin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	var pc *config.Provider
	cfg := s.getCfg()
	for i := range cfg.Providers {
		if cfg.Providers[i].Name == name {
			pc = &cfg.Providers[i]
			break
		}
	}
	if pc == nil || pc.Type != "antigravity" {
		http.NotFound(w, r)
		return
	}

	state := randomHex(16)
	http.SetCookie(w, &http.Cookie{Name: "omnigo_oauth_state", Value: state, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: "omnigo_oauth_provider", Value: name, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})

	redirectURI := antigravity.DefaultRedirectURI
	http.Redirect(w, r, antigravity.BuildAuthorizeURL(redirectURI, state), http.StatusFound)
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	stateCookie, _ := r.Cookie("omnigo_oauth_state")
	providerCookie, _ := r.Cookie("omnigo_oauth_provider")
	if stateCookie == nil || providerCookie == nil || r.URL.Query().Get("state") != stateCookie.Value {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	name := providerCookie.Value
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	redirectURI := scheme(r) + "://" + r.Host + "/oauth/callback"
	tok, err := s.exchange(r, code, redirectURI)
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	projectID, err := s.discover(r, tok.AccessToken)
	if err != nil {
		projectID = ""
	}

	err = s.store.Update(func(v *vault.Vault) error {
		sec := v.ProviderSecrets[name]
		sec.AccessToken = tok.AccessToken
		sec.RefreshToken = tok.RefreshToken
		sec.ExpiresAt = tok.ExpiresAt
		sec.ProjectID = projectID
		v.ProviderSecrets[name] = sec
		return nil
	})
	if err != nil {
		http.Error(w, "failed to persist credentials", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Connected</h1><p>Provider "` + name + `" is now connected. <a href="/">Back to dashboard</a></p></body></html>`))
}

// oauthPasteCallback processes a user-pasted callback URL or raw code.
func (s *Server) oauthPasteCallback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	var pc *config.Provider
	cfg := s.getCfg()
	for i := range cfg.Providers {
		if cfg.Providers[i].Name == name {
			pc = &cfg.Providers[i]
			break
		}
	}
	if pc == nil || pc.Type != "antigravity" {
		http.NotFound(w, r)
		return
	}

	rawInput := r.FormValue("callback")
	codeField := r.FormValue("code")

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

	err = s.store.Update(func(v *vault.Vault) error {
		sec := v.ProviderSecrets[name]
		sec.AccessToken = tok.AccessToken
		sec.RefreshToken = tok.RefreshToken
		sec.ExpiresAt = tok.ExpiresAt
		sec.ProjectID = projectID
		v.ProviderSecrets[name] = sec
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
				<span><strong>Antigravity Connected!</strong> Account authorized (Project: ` + projectID + `)</span>
			</div>
			<button class="btn btn-ghost btn-xs" onclick="this.closest('.alert').remove()">✕</button>
		</div>
	`))
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
