package dashboard

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider/antigravity"
	"github.com/ac-kurniawan/omnigo/internal/provider/codex"
	"github.com/ac-kurniawan/omnigo/internal/quota"
	"github.com/ac-kurniawan/omnigo/internal/vault"
	"github.com/ac-kurniawan/omnigo/internal/version"
)

//go:embed templates/*
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

type viewData struct {
	Providers []config.Provider
	Combos    []config.Combo
	Keys      []vault.ClientKey
	Secrets   map[string]vault.ProviderSecret
	Accounts  map[string][]vault.ProviderSecret
	NewKey    string
	CSRFToken string
	Tracker   *combo.Tracker
	Version   string
	Quota     *quota.Cache
}

type oauthPendingState struct {
	provider  string
	verifier  string
	createdAt time.Time
}

type Server struct {
	getCfg  func() *config.Config
	store   *vault.Store
	mutate  config.MutateFunc
	tmpl    *template.Template
	tracker *combo.Tracker
	version string

	// quotaCache backs the per-account quota badges. It is optional: without
	// it the providers view renders exactly as before.
	quotaCache *quota.Cache

	// exchange/discover are injectable OAuth seams (defaults to the
	// antigravity package); tests override them with fakes.
	exchange         func(r *http.Request, code, redirectURI string) (*antigravity.Token, error)
	discover         func(r *http.Request, accessToken string) (string, error)
	discoverIdentity func(r *http.Request, accessToken string) (antigravity.Identity, error)

	// codexExchange is the injectable token-exchange seam for the Codex
	// provider (defaults to the codex package).
	codexExchange func(r *http.Request, code, verifier, redirectURI string) (*codex.Token, error)

	oauthMu      sync.Mutex
	oauthPending map[string]oauthPendingState
}

func NewHandler(getCfg func() *config.Config, store *vault.Store, mutate config.MutateFunc, tracker ...*combo.Tracker) http.Handler {
	return dashboardHandler(getCfg, store, mutate, nil, tracker...)
}

// NewHandlerWithQuota is NewHandler plus the shared quota cache, which backs
// the per-account quota badges. Passing a nil cache is equivalent to
// NewHandler.
func NewHandlerWithQuota(getCfg func() *config.Config, store *vault.Store, mutate config.MutateFunc, cache *quota.Cache, tracker ...*combo.Tracker) http.Handler {
	return dashboardHandler(getCfg, store, mutate, cache, tracker...)
}

// dashboardHandler wires the dashboard surface once, so both constructors
// share one definition of the enabled gate and the basic-auth wrapper.
func dashboardHandler(getCfg func() *config.Config, store *vault.Store, mutate config.MutateFunc, cache *quota.Cache, tracker ...*combo.Tracker) http.Handler {
	var tr *combo.Tracker
	if len(tracker) > 0 {
		tr = tracker[0]
	}
	server := newServer(getCfg, store, mutate, tr)
	server.quotaCache = cache
	inner := auth.DashboardBasicAuth(func() bool {
		cfg := getCfg()
		return cfg == nil || cfg.Dashboard.AuthEnabled()
	})(server.routes())

	// The enabled gate is read from the live config on every request, so
	// toggling dashboard.enabled in config.yaml takes effect on reload without
	// a restart. While disabled the surface is indistinguishable from an
	// unregistered path: 404 before basic auth is consulted.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg := getCfg(); cfg != nil && !cfg.Dashboard.IsEnabled() {
			http.NotFound(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func newServer(getCfg func() *config.Config, store *vault.Store, mutate config.MutateFunc, tracker ...*combo.Tracker) *Server {
	var tr *combo.Tracker
	if len(tracker) > 0 {
		tr = tracker[0]
	}
	tmpl := template.Must(template.New("root").Funcs(template.FuncMap{
		"sub": func(a, b int) int { return a - b },
		"isDrained": func(tr *combo.Tracker, provider, model string) bool {
			if tr == nil {
				return false
			}
			return tr.IsDrained(combo.Target{Provider: provider, Model: model})
		},
		"drainRemaining": func(tr *combo.Tracker, provider, model string) string {
			if tr == nil {
				return ""
			}
			rem := tr.DrainRemaining(combo.Target{Provider: provider, Model: model})
			if rem <= 0 {
				return ""
			}
			rem = rem.Round(time.Second)
			m := int(rem.Minutes())
			s := int(rem.Seconds()) % 60
			return fmt.Sprintf("%dm%02ds", m, s)
		},
		"drainReason": func(tr *combo.Tracker, provider, model string) string {
			if tr == nil {
				return ""
			}
			return tr.DrainReason(combo.Target{Provider: provider, Model: model})
		},
		"providerSecret": func(v viewData, name string) vault.ProviderSecret {
			if secret, ok := v.Secrets[name]; ok {
				return secret
			}
			if accounts := v.Accounts[name]; len(accounts) > 0 {
				return accounts[0]
			}
			return vault.ProviderSecret{}
		},
		"accountIdentity": func(account vault.ProviderSecret) string {
			return account.Identity()
		},
		"urlEscape": func(s string) string {
			return url.PathEscape(s)
		},
		"accountHealthy": func(account vault.ProviderSecret) bool {
			if account.AccessToken == "" && account.RefreshToken == "" {
				return false
			}
			return account.ExpiresAt.IsZero() || account.RefreshToken != "" || time.Until(account.ExpiresAt) > 0
		},
		"accountStatus": func(account vault.ProviderSecret) string {
			if account.AccessToken == "" && account.RefreshToken == "" {
				return "Reconnect required"
			}
			if !account.ExpiresAt.IsZero() && time.Until(account.ExpiresAt) <= 0 && account.RefreshToken == "" {
				return "Expired"
			}
			return "Ready"
		},
		"accountStatusClass": func(account vault.ProviderSecret) string {
			if account.AccessToken == "" && account.RefreshToken == "" {
				return "status-pill-error"
			}
			if !account.ExpiresAt.IsZero() && time.Until(account.ExpiresAt) <= 0 && account.RefreshToken == "" {
				return "status-pill-warning"
			}
			return "status-pill-neutral"
		},
		"drainedCount": func(tr *combo.Tracker) int {
			if tr == nil {
				return 0
			}
			return tr.DrainedCount()
		},
		// quotaSnapshot returns the cached quota for one credential, or nil
		// when no poll has completed yet. The template renders a badge only
		// when a snapshot exists, so an unpolled account shows no state rather
		// than a misleading "available".
		"quotaSnapshot": func(v viewData, provider string, account vault.ProviderSecret) *quota.AccountSnapshot {
			if v.Quota == nil {
				return nil
			}
			snapshot, ok := v.Quota.Get(provider, account.Identity())
			if !ok {
				return nil
			}
			return &snapshot
		},
		"quotaHeadline": func(snapshot *quota.AccountSnapshot) string {
			if snapshot == nil {
				return ""
			}
			return snapshot.Headline(time.Now())
		},
		"quotaStatusClass": func(snapshot *quota.AccountSnapshot) string {
			if snapshot == nil {
				return "status-pill-neutral"
			}
			switch snapshot.Status {
			case quota.StatusAvailable:
				return "status-pill-success"
			case quota.StatusExhausted:
				return "status-pill-error"
			default:
				return "status-pill-warning"
			}
		},
		"quotaStatusLabel": func(snapshot *quota.AccountSnapshot) string {
			if snapshot == nil {
				return ""
			}
			switch snapshot.Status {
			case quota.StatusAvailable:
				return "Available"
			case quota.StatusExhausted:
				return "Exhausted"
			default:
				return "Unavailable"
			}
		},
		"quotaWindowLabel": func(w quota.Window) string {
			return w.Label()
		},
		"quotaFormatPercent": func(p float64) string {
			if p == float64(int(p)) {
				return fmt.Sprintf("%d%%", int(p))
			}
			return fmt.Sprintf("%.1f%%", p)
		},
		"quotaTopWindows": func(snapshot *quota.AccountSnapshot, n int) []quota.Window {
			if snapshot == nil {
				return nil
			}
			return snapshot.TopWindows(n)
		},
		"quotaSortedWindows": func(snapshot *quota.AccountSnapshot) []quota.Window {
			if snapshot == nil {
				return nil
			}
			return snapshot.SortedWindows()
		},
		"quotaWindowProgressClass": func(w quota.Window) string {
			rem := w.RemainingPercent()
			switch {
			case rem < 10:
				return "progress-error"
			case rem <= 30:
				return "progress-warning"
			default:
				return "progress-success"
			}
		},
		"quotaObservedAgo": func(snapshot *quota.AccountSnapshot) string {
			if snapshot == nil || snapshot.ObservedAt.IsZero() {
				return ""
			}
			d := time.Since(snapshot.ObservedAt).Round(time.Second)
			if d < time.Minute {
				return "just now"
			}
			m := int(d.Minutes())
			if m < 60 {
				return fmt.Sprintf("%dm ago", m)
			}
			h := int(d.Hours())
			return fmt.Sprintf("%dh ago", h)
		},
		"quotaResetIn": func(w quota.Window) string {
			if w.ResetAt.IsZero() {
				return ""
			}
			rem := time.Until(w.ResetAt).Round(time.Minute)
			if rem <= 0 {
				return "now"
			}
			h := int(rem.Hours())
			m := int(rem.Minutes()) % 60
			if h > 24 {
				return fmt.Sprintf("%dd%dh", h/24, h%24)
			}
			if h > 0 {
				return fmt.Sprintf("%dh%dm", h, m)
			}
			return fmt.Sprintf("%dm", m)
		},
		"quotaModalID": func(provider, identity string) string {
			// Derive a stable HTML-safe ID for the modal dialog and the
			// aria-labelledby reference to its title. Whitespace must go too:
			// aria-labelledby is a whitespace-separated id list, so a value
			// containing a space could never reference the element.
			safe := strings.Map(func(r rune) rune {
				switch r {
				case ':', '@', '+', '.', ' ', '\t', '\n', '\r':
					return '-'
				}
				return r
			}, provider+"-"+identity)
			return "quota-modal-" + safe
		},
	}).ParseFS(templatesFS, "templates/*.html"))

	s := &Server{
		getCfg:  getCfg,
		store:   store,
		mutate:  mutate,
		tmpl:    tmpl,
		tracker: tr,
		version: version.Value,
	}
	s.exchange = func(r *http.Request, code, redirectURI string) (*antigravity.Token, error) {
		return antigravity.ExchangeCode(r.Context(), code, redirectURI)
	}
	s.discover = func(r *http.Request, accessToken string) (string, error) {
		return antigravity.DiscoverProject(r.Context(), accessToken)
	}
	s.discoverIdentity = func(r *http.Request, accessToken string) (antigravity.Identity, error) {
		return antigravity.DiscoverIdentity(r.Context(), accessToken)
	}
	s.codexExchange = func(r *http.Request, code, verifier, redirectURI string) (*codex.Token, error) {
		return codex.ExchangeCode(r.Context(), code, verifier, redirectURI)
	}
	s.oauthPending = make(map[string]oauthPendingState)
	return s
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /oauth/login/{provider}", s.oauthLogin)
	mux.HandleFunc("GET /oauth/callback", s.oauthCallback)
	mux.HandleFunc("POST /oauth/{provider}/paste-callback", s.oauthPasteCallback)
	mux.HandleFunc("GET /providers", s.getProviders)
	mux.HandleFunc("POST /providers", s.createProvider)
	mux.HandleFunc("POST /providers/{name}/key", s.setProviderKey)
	mux.HandleFunc("POST /providers/{name}/toggle", s.toggleProvider)
	mux.HandleFunc("POST /providers/{name}/delete", s.deleteProvider)
	mux.HandleFunc("POST /providers/{name}/accounts/{identity}/delete", s.deleteProviderAccount)
	mux.HandleFunc("POST /providers/{name}/models", s.addModel)
	mux.HandleFunc("POST /providers/{name}/models/disable", s.disableModel)
	mux.HandleFunc("POST /providers/{name}/models/enable", s.enableModel)
	mux.HandleFunc("POST /providers/{name}/models/delete", s.deleteModel)
	mux.HandleFunc("GET /combos", s.getCombos)
	mux.HandleFunc("POST /combos", s.createCombo)
	mux.HandleFunc("POST /combos/{name}", s.updateCombo)
	mux.HandleFunc("POST /combos/{name}/delete", s.deleteCombo)
	mux.HandleFunc("POST /combos/drains/reset", s.resetDrain)
	mux.HandleFunc("POST /combos/drains/reset-all", s.resetAllDrains)
	mux.HandleFunc("GET /keys", s.getKeys)
	mux.HandleFunc("POST /keys", s.createKey)
	mux.HandleFunc("POST /keys/{id}/revoke", s.revokeKey)
	return mux
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	cfg := s.getCfg()
	snapshot := s.store.Get()
	token := randomHex(32)
	http.SetCookie(w, &http.Cookie{Name: "omnigo_csrf", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	data := viewData{
		Providers: cfg.Providers,
		Combos:    cfg.Combos,
		Keys:      activeKeys(snapshot.ClientKeys),
		Secrets:   snapshot.ProviderSecrets,
		Accounts:  snapshot.ProviderAccounts,
		CSRFToken: token,
		Tracker:   s.tracker,
		Version:   s.version,
		Quota:     s.quotaCache,
	}
	_ = s.tmpl.ExecuteTemplate(w, "index.html", data)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(b)
}

func (s *Server) recordOAuthPending(state, provider, verifier string) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.oauthPending == nil {
		s.oauthPending = make(map[string]oauthPendingState)
	}
	now := time.Now()
	for k, v := range s.oauthPending {
		if now.Sub(v.createdAt) > 15*time.Minute {
			delete(s.oauthPending, k)
		}
	}
	// Hard ceiling to prevent memory exhaustion under floods
	if len(s.oauthPending) >= 1000 {
		for k := range s.oauthPending {
			delete(s.oauthPending, k)
			if len(s.oauthPending) < 500 {
				break
			}
		}
	}
	s.oauthPending[state] = oauthPendingState{
		provider:  provider,
		verifier:  verifier,
		createdAt: now,
	}
}

func (s *Server) consumeOAuthPending(state string) (provider, verifier string, ok bool) {
	if state == "" {
		return "", "", false
	}
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	flow, found := s.oauthPending[state]
	if !found {
		return "", "", false
	}
	delete(s.oauthPending, state)
	if time.Since(flow.createdAt) > 15*time.Minute {
		return "", "", false
	}
	return flow.provider, flow.verifier, true
}
