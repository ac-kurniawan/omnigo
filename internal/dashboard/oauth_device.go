package dashboard

import (
	"crypto/subtle"
	"fmt"
	"html"
	"net/http"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/vault"
)

const deviceCookieName = "omnigo_muse_device_flow"

func (s *Server) checkCSRF(r *http.Request) bool {
	cookie, err := r.Cookie("omnigo_csrf")
	if err != nil || cookie.Value == "" {
		return false
	}
	hdr := r.Header.Get("X-Omnigo-CSRF")
	if hdr == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(hdr)) == 1
}

func (s *Server) deviceOAuthStart(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	name := r.PathValue("provider")
	pc := s.findProvider(name)
	if pc == nil || pc.Type != "muse" {
		http.NotFound(w, r)
		return
	}

	if s.museClient == nil {
		http.Error(w, "muse oauth client not available", http.StatusInternalServerError)
		return
	}

	resp, err := s.museClient.RequestDeviceCode(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `<div class="alert alert-error text-xs p-3">Failed to request device authorization code.</div>`)
		return
	}

	flowID := randomHex(24)
	interval := resp.Interval
	if interval < 5 {
		interval = 5
	}
	expiresIn := resp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}

	verifyURI := resp.VerificationURI
	if verifyURI == "" {
		verifyURI = "https://auth.meta.com/device"
	}

	now := time.Now()
	s.oauthMu.Lock()
	if s.devicePending == nil {
		s.devicePending = make(map[string]devicePendingState)
	}
	for k, v := range s.devicePending {
		if now.After(v.expiresAt) || now.Sub(v.createdAt) > 15*time.Minute {
			delete(s.devicePending, k)
		}
	}
	s.devicePending[flowID] = devicePendingState{
		provider:   name,
		flowID:     flowID,
		deviceCode: resp.DeviceCode,
		userCode:   resp.UserCode,
		verifyURI:  verifyURI,
		interval:   interval,
		expiresAt:  now.Add(time.Duration(expiresIn) * time.Second),
		createdAt:  now,
	}
	s.oauthMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     deviceCookieName,
		Value:    flowID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `
<div class="space-y-4">
  <div class="bg-base-200/80 p-4 rounded-xl border border-base-content/10 text-center">
    <div class="text-xs text-base-content/60 font-medium mb-1">Enter code at Meta Device Login</div>
    <div class="font-mono text-2xl font-bold tracking-wider text-primary py-2 select-all">%s</div>
    <div class="mt-2 flex justify-center gap-2">
      <a href="%s" target="_blank" rel="noopener noreferrer" class="btn btn-sm btn-primary gap-1.5">
        Open Meta Login
        <svg xmlns="http://www.w3.org/2000/svg" class="size-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M10 6H6a2 2 0 00-2 2v10a2 2 0 002 2h10a2 2 0 002-2v-4M14 4h6m0 0v6m0-6L10 14"/></svg>
      </a>
      <button type="button" class="btn btn-sm btn-outline" onclick="copyToClipboard(this.getAttribute('data-code'), this)" data-code="%s">Copy Code</button>
    </div>
  </div>
  <div id="device-poll-status-%s"
       hx-get="/providers/%s/oauth/device/poll"
       hx-trigger="every %ds"
       hx-swap="outerHTML"
       class="flex items-center justify-center gap-2 text-xs text-base-content/70 py-2">
    <span class="loading loading-spinner loading-xs text-primary"></span>
    Waiting for authorization on Meta...
  </div>
</div>`,
		html.EscapeString(resp.UserCode),
		html.EscapeString(verifyURI),
		html.EscapeString(resp.UserCode),
		html.EscapeString(name),
		html.EscapeString(name),
		interval,
	)
}

func (s *Server) deviceOAuthPoll(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	pc := s.findProvider(name)
	if pc == nil || pc.Type != "muse" {
		http.NotFound(w, r)
		return
	}

	cookie, _ := r.Cookie(deviceCookieName)
	if cookie == nil || cookie.Value == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="alert alert-warning text-xs p-3">Device authorization session not found. Please click Connect Meta again.</div>`)
		return
	}
	flowID := cookie.Value

	s.oauthMu.Lock()
	state, found := s.devicePending[flowID]
	if !found || state.provider != name {
		s.oauthMu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="alert alert-warning text-xs p-3">Device authorization session expired. Please click Connect Meta again.</div>`)
		return
	}

	if time.Now().After(state.expiresAt) {
		delete(s.devicePending, flowID)
		s.oauthMu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="alert alert-warning text-xs p-3">Device code expired. Please click Connect Meta again.</div>`)
		return
	}

	if state.done {
		s.oauthMu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="alert alert-success text-xs p-3">✓ Connected successfully!</div>`)
		return
	}

	if state.inflight {
		s.oauthMu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `
  <div id="device-poll-status-%s"
       hx-get="/providers/%s/oauth/device/poll"
       hx-trigger="every %ds"
       hx-swap="outerHTML"
       class="flex items-center justify-center gap-2 text-xs text-base-content/70 py-2">
    <span class="loading loading-spinner loading-xs text-primary"></span>
    Waiting for authorization on Meta...
  </div>`, html.EscapeString(name), html.EscapeString(name), state.interval)
		return
	}

	state.inflight = true
	s.devicePending[flowID] = state
	s.oauthMu.Unlock()

	defer func() {
		s.oauthMu.Lock()
		if cur, ok := s.devicePending[flowID]; ok {
			cur.inflight = false
			s.devicePending[flowID] = cur
		}
		s.oauthMu.Unlock()
	}()

	pollRes, err := s.museClient.PollToken(r.Context(), state.deviceCode)
	if err != nil {
		s.oauthMu.Lock()
		delete(s.devicePending, flowID)
		s.oauthMu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="alert alert-error text-xs p-3">Authorization failed. Please try again.</div>`)
		return
	}

	if pollRes.Pending {
		s.oauthMu.Lock()
		interval := state.interval
		if pollRes.Error == "slow_down" {
			interval += 5
			state.interval = interval
			s.devicePending[flowID] = state
		}
		s.oauthMu.Unlock()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `
  <div id="device-poll-status-%s"
       hx-get="/providers/%s/oauth/device/poll"
       hx-trigger="every %ds"
       hx-swap="outerHTML"
       class="flex items-center justify-center gap-2 text-xs text-base-content/70 py-2">
    <span class="loading loading-spinner loading-xs text-primary"></span>
    Waiting for authorization on Meta...
  </div>`, html.EscapeString(name), html.EscapeString(name), interval)
		return
	}

	mintResp, err := s.museClient.MintSubscriptionKey(r.Context(), pollRes.AccessToken)
	if err != nil {
		s.oauthMu.Lock()
		delete(s.devicePending, flowID)
		s.oauthMu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="alert alert-error text-xs p-3">Subscription key minting failed. Please check your Muse subscription.</div>`)
		return
	}

	err = s.store.Update(func(v *vault.Vault) error {
		v.UpsertAccount(name, vault.ProviderSecret{
			APIKey:    mintResp.APIKey,
			AccountID: mintResp.SubsTierName,
			Email:     mintResp.UserEmail,
		})
		return nil
	})
	if err != nil {
		s.oauthMu.Lock()
		delete(s.devicePending, flowID)
		s.oauthMu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `<div class="alert alert-error text-xs p-3">Failed to save credentials.</div>`)
		return
	}

	s.oauthMu.Lock()
	state.done = true
	state.inflight = false
	s.devicePending[flowID] = state
	s.oauthMu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `
<div class="alert alert-success text-xs p-3">
  ✓ Connected successfully! Account linked.
</div>
<script>
  setTimeout(() => {
    const modal = document.getElementById('device-modal-%s');
    if (modal) modal.close();
    if (window.htmx) htmx.ajax('GET', '/providers', {target:'#providers', swap:'innerHTML'});
  }, 1200);
</script>`,
		html.EscapeString(name),
	)
}
