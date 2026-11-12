package dashboard

import (
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider/commandcode"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

const (
	commandCodeStudioURL    = "https://commandcode.ai/studio/auth/cli"
	commandCodeStudioOrigin = "https://commandcode.ai"
	commandCodePendingTTL   = 10 * time.Minute
)

type commandCodePendingEntry struct {
	provider string
	status   string
	message  string
	// key is the first accepted API key for this state. A repeat callback
	// with a different key is rejected so a leaked state cannot rotate the
	// stored credential within the pending window.
	key     string
	expires time.Time
}

// commandCodeCallbackURL builds the local callback Studio POSTs the key to.
func (s *Server) commandCodeCallbackURL(r *http.Request, name string) string {
	return scheme(r) + "://" + r.Host + "/oauth/commandcode/callback"
}

func (s *Server) commandCodeStudioLink(r *http.Request, name, state string) string {
	callback := s.commandCodeCallbackURL(r, name)
	return commandCodeStudioURL + "?callback=" + url.QueryEscape(callback) + "&state=" + url.QueryEscape(state)
}

// commandCodeLogin renders the browser sign-in waiting page for commandcode
// providers. Studio opens in a new tab and POSTs the issued key back to the
// local callback; the page polls the pending endpoint until it flips.
func (s *Server) commandCodeLogin(w http.ResponseWriter, r *http.Request, name string) {
	state := randomHex(16)
	s.recordOAuthPending(state, name, "")
	s.setCommandCodePending(state, name, "waiting", "")
	link := s.commandCodeStudioLink(r, name, state)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><title>Connect CommandCode</title></head><body>` +
		`<h1>Connect CommandCode</h1>` +
		`<p>Provider "` + html.EscapeString(name) + `" — sign in with CommandCode Studio, then approve the CLI key.</p>` +
		`<p><a href="` + html.EscapeString(link) + `" target="_blank" rel="noopener">Open CommandCode Studio (new tab)</a></p>` +
		`<p id="cc-status">Waiting for Studio approval…</p>` +
		`<script>var poll=setInterval(function(){fetch("/oauth/commandcode/pending?state=` + url.QueryEscape(state) + `").then(function(r){return r.json()}).then(function(j){var el=document.getElementById("cc-status");if(j.status){el.textContent=j.status}if(j.status==="connected"){clearInterval(poll);el.textContent="Connected — you can close this tab."}if(j.status==="failed"){clearInterval(poll);el.textContent="Failed: "+(j.message||"unknown")}}).catch(function(){})},2000);</script>` +
		`</body></html>`))
}

func (s *Server) setCommandCodePending(state, provider, status, message string) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.commandCodePending == nil {
		s.commandCodePending = make(map[string]commandCodePendingEntry)
	}
	now := time.Now()
	for k, v := range s.commandCodePending {
		if now.After(v.expires) {
			delete(s.commandCodePending, k)
		}
	}
	key := ""
	if prev, ok := s.commandCodePending[state]; ok && prev.provider == provider {
		key = prev.key
	}
	s.commandCodePending[state] = commandCodePendingEntry{provider: provider, status: status, message: message, key: key, expires: now.Add(commandCodePendingTTL)}
}

// bindCommandCodeKey records the first accepted key for a pending state so a
// repeat callback with a different key is rejected.
func (s *Server) bindCommandCodeKey(state, key string) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	e, ok := s.commandCodePending[state]
	if !ok || e.key != "" {
		return
	}
	e.key = key
	s.commandCodePending[state] = e
}

func (s *Server) getCommandCodePending(state string) (commandCodePendingEntry, bool) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	e, ok := s.commandCodePending[state]
	if !ok {
		return commandCodePendingEntry{}, false
	}
	if time.Now().After(e.expires) {
		delete(s.commandCodePending, state)
		return commandCodePendingEntry{}, false
	}
	return e, true
}

func (s *Server) consumeCommandCodePending(state string) (commandCodePendingEntry, bool) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	e, ok := s.commandCodePending[state]
	if !ok {
		return commandCodePendingEntry{}, false
	}
	delete(s.commandCodePending, state)
	if time.Now().After(e.expires) {
		return commandCodePendingEntry{}, false
	}
	return e, true
}

// oauthCommandCodeCallback receives the Studio-issued key. Studio POSTs JSON
// from the browser; a GET fallback covers manual paste flows.
func (s *Server) oauthCommandCodeCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", commandCodeStudioOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var apiKey, state, userID, userName string
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		apiKey = q.Get("apiKey")
		state = q.Get("state")
		userID = q.Get("userId")
		userName = q.Get("userName")
	} else if r.Method == http.MethodPost {
		var payload struct {
			APIKey   string `json:"apiKey"`
			State    string `json:"state"`
			UserID   string `json:"userId"`
			UserName string `json:"userName"`
			KeyName  string `json:"keyName"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid callback payload", http.StatusBadRequest)
			return
		}
		apiKey, state, userID, userName = payload.APIKey, payload.State, payload.UserID, payload.UserName
	} else {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pending, ok := s.getCommandCodePending(state)
	if !ok || pending.provider == "" {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	name := pending.provider
	pc := s.findProvider(name)
	if pc == nil || pc.Type != "commandcode" {
		http.Error(w, "provider mismatch", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(apiKey)
	if key == "" {
		s.setCommandCodePending(state, name, "failed", "empty API key")
		http.Error(w, "missing apiKey", http.StatusBadRequest)
		return
	}
	if pending.key != "" && pending.key != key {
		http.Error(w, "state already used", http.StatusConflict)
		return
	}
	identity, err := s.commandcodeValidate(r.Context(), commandcode.APIBaseFor(pc.BaseURL), key)
	if err != nil {
		s.setCommandCodePending(state, name, "failed", "validation failed")
		http.Error(w, "key validation failed", http.StatusBadGateway)
		return
	}
	if userID == "" {
		userID = identity.UserID
	}
	if userName == "" {
		userName = identity.UserName
	}
	email := identity.Email
	if email == "" {
		email = userName
	}
	if err := s.store.Update(func(v *vault.Vault) error {
		v.UpsertAccount(name, vault.ProviderSecret{
			APIKey: key, AccountID: userID, Email: email, ProjectID: identity.OrgID,
		})
		return nil
	}); err != nil {
		s.setCommandCodePending(state, name, "failed", "persist failed")
		http.Error(w, "failed to persist credentials", http.StatusInternalServerError)
		return
	}
	s.setCommandCodePending(state, name, "connected", "")
	s.bindCommandCodeKey(state, key)
	w.Header().Set("Access-Control-Allow-Origin", commandCodeStudioOrigin)
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Connected</h1><p>Provider "` + html.EscapeString(name) + `" is now connected. <a href="/">Back to dashboard</a></p></body></html>`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"success":true}`))
}

// oauthCommandCodePending reports the browser-flow status for one state. It
// carries no secrets.
func (s *Server) oauthCommandCodePending(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	pending, ok := s.getCommandCodePending(state)
	if !ok {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	if pending.status == "connected" || pending.status == "failed" {
		s.consumeCommandCodePending(state)
	}
	w.Header().Set("Content-Type", "application/json")
	out := map[string]string{"status": pending.status}
	if pending.message != "" {
		out["message"] = pending.message
	}
	_ = json.NewEncoder(w).Encode(out)
}

// commandCodePasteCallback handles a pasted raw key for commandcode
// providers, using the same validate+persist path as the browser callback.
func (s *Server) commandCodePasteCallback(w http.ResponseWriter, r *http.Request, name, rawInput, codeField string) {
	key := strings.TrimSpace(codeField)
	if key == "" {
		key = strings.TrimSpace(rawInput)
	}
	if strings.HasPrefix(key, "http://") || strings.HasPrefix(key, "https://") {
		if u, err := url.Parse(key); err == nil {
			if k := u.Query().Get("apiKey"); k != "" {
				key = k
			} else if k := u.Query().Get("code"); k != "" {
				key = k
			}
		}
	}
	if key == "" {
		http.Error(w, "missing API key", http.StatusBadRequest)
		return
	}
	pc := s.findProvider(name)
	if pc == nil || pc.Type != "commandcode" {
		http.NotFound(w, r)
		return
	}
	identity, err := s.commandcodeValidate(r.Context(), commandcode.APIBaseFor(pc.BaseURL), key)
	if err != nil {
		http.Error(w, "key validation failed", http.StatusBadGateway)
		return
	}
	email := identity.Email
	if email == "" {
		email = identity.UserName
	}
	if err := s.store.Update(func(v *vault.Vault) error {
		v.UpsertAccount(name, vault.ProviderSecret{
			APIKey: key, AccountID: identity.UserID, Email: email, ProjectID: identity.OrgID,
		})
		return nil
	}); err != nil {
		http.Error(w, "failed to persist credentials", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`
		<div class="alert alert-success shadow-lg flex items-center justify-between p-4 rounded-xl border border-success/30">
			<div class="flex items-center gap-2.5">
				<svg xmlns="http://www.w3.org/2000/svg" class="size-5 shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" /></svg>
				<span class="text-sm"><strong>CommandCode Connected!</strong> Account authorized` + commandCodeAccountSuffix(identity) + `</span>
			</div>
			<button class="btn btn-ghost btn-xs" onclick="this.closest('.alert').remove()">✕</button>
		</div>
	`))
}

func commandCodeAccountSuffix(identity commandcode.WhoAmI) string {
	label := identity.UserName
	if label == "" {
		label = identity.Email
	}
	if label == "" {
		return ""
	}
	return " (" + html.EscapeString(label) + ")"
}
