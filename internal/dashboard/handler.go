package dashboard

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"html/template"
	"net/http"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider/antigravity"
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
}

type Server struct {
	getCfg func() *config.Config
	store  *vault.Store
	mutate config.MutateFunc
	tmpl   *template.Template

	// exchange/discover are injectable OAuth seams (defaults to the
	// antigravity package); tests override them with fakes.
	exchange func(r *http.Request, code, redirectURI string) (*antigravity.Token, error)
	discover func(r *http.Request, accessToken string) (string, error)
}

func NewHandler(getCfg func() *config.Config, store *vault.Store, mutate config.MutateFunc) http.Handler {
	return newServer(getCfg, store, mutate).routes()
}

func newServer(getCfg func() *config.Config, store *vault.Store, mutate config.MutateFunc) *Server {
	tmpl := template.Must(template.New("root").Funcs(template.FuncMap{
		"sub": func(a, b int) int { return a - b },
	}).ParseFS(templatesFS, "templates/*.html"))

	s := &Server{
		getCfg: getCfg,
		store:  store,
		mutate: mutate,
		tmpl:   tmpl,
	}
	s.exchange = func(r *http.Request, code, redirectURI string) (*antigravity.Token, error) {
		return antigravity.ExchangeCode(r.Context(), code, redirectURI)
	}
	s.discover = func(r *http.Request, accessToken string) (string, error) {
		return antigravity.DiscoverProject(r.Context(), accessToken)
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
	mux.HandleFunc("POST /combos/{name}/delete", s.deleteCombo)
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
		Keys:      s.store.Get().ClientKeys,
		Secrets:   s.store.Get().ProviderSecrets,
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
