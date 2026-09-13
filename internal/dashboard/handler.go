package dashboard

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider/antigravity"
	"github.com/ac-kurniawan/omnigo/internal/provider/codex"
	"github.com/ac-kurniawan/omnigo/internal/vault"
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
	NewKey    string
	Tracker   *combo.Tracker
}

type Server struct {
	getCfg  func() *config.Config
	store   *vault.Store
	mutate  config.MutateFunc
	tmpl    *template.Template
	tracker *combo.Tracker

	// exchange/discover are injectable OAuth seams (defaults to the
	// antigravity package); tests override them with fakes.
	exchange func(r *http.Request, code, redirectURI string) (*antigravity.Token, error)
	discover func(r *http.Request, accessToken string) (string, error)

	// codexExchange is the injectable token-exchange seam for the Codex
	// provider (defaults to the codex package).
	codexExchange func(r *http.Request, code, verifier, redirectURI string) (*codex.Token, error)
}

func NewHandler(getCfg func() *config.Config, store *vault.Store, mutate config.MutateFunc, tracker ...*combo.Tracker) http.Handler {
	var tr *combo.Tracker
	if len(tracker) > 0 {
		tr = tracker[0]
	}
	return newServer(getCfg, store, mutate, tr).routes()
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
		"drainedCount": func(tr *combo.Tracker) int {
			if tr == nil {
				return 0
			}
			return tr.DrainedCount()
		},
	}).ParseFS(templatesFS, "templates/*.html"))

	s := &Server{
		getCfg:  getCfg,
		store:   store,
		mutate:  mutate,
		tmpl:    tmpl,
		tracker: tr,
	}
	s.exchange = func(r *http.Request, code, redirectURI string) (*antigravity.Token, error) {
		return antigravity.ExchangeCode(r.Context(), code, redirectURI)
	}
	s.discover = func(r *http.Request, accessToken string) (string, error) {
		return antigravity.DiscoverProject(r.Context(), accessToken)
	}
	s.codexExchange = func(r *http.Request, code, verifier, redirectURI string) (*codex.Token, error) {
		return codex.ExchangeCode(r.Context(), code, verifier, redirectURI)
	}
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
	data := viewData{
		Providers: cfg.Providers,
		Combos:    cfg.Combos,
		Keys:      activeKeys(s.store.Get().ClientKeys),
		Secrets:   s.store.Get().ProviderSecrets,
		Tracker:   s.tracker,
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
