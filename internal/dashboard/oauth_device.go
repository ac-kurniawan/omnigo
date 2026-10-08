package dashboard

import (
	"fmt"
	"html"
	"net/http"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func (s *Server) deviceOAuthStart(w http.ResponseWriter, r *http.Request) {
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
		fmt.Fprintf(w, `<div class="alert alert-error text-xs p-3">Failed to request device code: %s</div>`, html.EscapeString(err.Error()))
		return
	}

	s.oauthMu.Lock()
	if s.devicePending == nil {
		s.devicePending = make(map[string]devicePendingState)
	}
	s.devicePending[name] = devicePendingState{
		provider:   name,
		deviceCode: resp.DeviceCode,
		createdAt:  time.Now(),
	}
	s.oauthMu.Unlock()

	verifyURI := resp.VerificationURI
	if verifyURI == "" {
		verifyURI = "https://auth.meta.com/device"
	}

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
      <button type="button" class="btn btn-sm btn-outline" onclick="copyToClipboard('%s', this)">Copy Code</button>
    </div>
  </div>
  <div id="device-poll-status-%s" 
       hx-get="/providers/%s/oauth/device/poll" 
       hx-trigger="every 5s" 
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
	)
}

func (s *Server) deviceOAuthPoll(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	pc := s.findProvider(name)
	if pc == nil || pc.Type != "muse" {
		http.NotFound(w, r)
		return
	}

	s.oauthMu.Lock()
	state, found := s.devicePending[name]
	s.oauthMu.Unlock()

	if !found || time.Since(state.createdAt) > 15*time.Minute {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="alert alert-warning text-xs p-3">Device authorization session expired or not found. Please click Connect Meta again.</div>`)
		return
	}

	pollRes, err := s.museClient.PollToken(r.Context(), state.deviceCode)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="alert alert-error text-xs p-3">Authorization error: %s</div>`, html.EscapeString(err.Error()))
		return
	}

	if pollRes.Pending {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `
  <div id="device-poll-status-%s" 
       hx-get="/providers/%s/oauth/device/poll" 
       hx-trigger="every 5s" 
       hx-swap="outerHTML" 
       class="flex items-center justify-center gap-2 text-xs text-base-content/70 py-2">
    <span class="loading loading-spinner loading-xs text-primary"></span>
    Waiting for authorization on Meta...
  </div>`, html.EscapeString(name), html.EscapeString(name))
		return
	}

	// Token acquired! Mint subscription key
	mintResp, err := s.museClient.MintSubscriptionKey(r.Context(), pollRes.AccessToken)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<div class="alert alert-error text-xs p-3">Key minting failed: %s</div>`, html.EscapeString(err.Error()))
		return
	}

	// Persist to vault
	err = s.store.Update(func(v *vault.Vault) error {
		v.UpsertAccount(name, vault.ProviderSecret{
			APIKey:    mintResp.APIKey,
			AccountID: mintResp.SubsTierName,
			Email:     mintResp.UserEmail,
		})
		return nil
	})
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `<div class="alert alert-error text-xs p-3">Failed to save credentials: %s</div>`, html.EscapeString(err.Error()))
		return
	}

	// Clear pending state
	s.oauthMu.Lock()
	delete(s.devicePending, name)
	s.oauthMu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `
<div class="alert alert-success text-xs p-3">
  ✓ Connected successfully! Account: %s (%s). Key minted and saved.
</div>
<script>
  setTimeout(() => {
    const modal = document.getElementById('device-modal-%s');
    if (modal) modal.close();
    if (window.htmx) htmx.ajax('GET', '/providers', {target:'#providers', swap:'innerHTML'});
  }, 1500);
</script>`,
		html.EscapeString(mintResp.UserEmail),
		html.EscapeString(mintResp.SubsTierName),
		html.EscapeString(name),
	)
}
