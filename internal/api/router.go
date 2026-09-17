package api

import (
	"net/http"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// NewRouter builds the gateway mux. m may be nil, in which case a no-op
// recorder is used.
func NewRouter(getCfg func() *config.Config, store *vault.Store, mutate config.MutateFunc, tracker *combo.Tracker, version string, m Metrics) http.Handler {
	m = orNoop(m)
	registry := newProviderRegistry(store)
	registry.ensure(getCfg())
	mux := http.NewServeMux()
	authed := auth.Middleware(store.Get)

	mux.HandleFunc("GET /health", handleHealth(version))
	mux.Handle("GET /actuator/metrics", metricsHandler(getCfg, m))

	// /v1/* is gated by client API keys.
	mux.Handle("POST /v1/chat/completions", authed(http.HandlerFunc(handleChat(getCfg, registry, tracker, m))))
	mux.Handle("GET /v1/models", authed(http.HandlerFunc(handleModels(getCfg))))
	mux.Handle("GET /v1/models/{model...}", authed(http.HandlerFunc(handleModel(getCfg))))

	mux.Handle("POST /internal/refresh-models/{provider}", dashboardEnabled(getCfg, auth.DashboardMiddleware(handleRefreshModels(getCfg, registry, mutate))))
	mux.Handle("POST /internal/test/{provider}", dashboardEnabled(getCfg, auth.DashboardMiddleware(handleTestProvider(getCfg, registry))))

	return mux
}

// dashboardEnabled gates the dashboard's /internal/* helpers on
// dashboard.enabled, mirroring the UI surface: while disabled the paths answer
// 404, matching an unregistered path, before dashboard authorization runs. The
// flag is read from the live config on every request so a reload applies
// without a restart.
func dashboardEnabled(getCfg func() *config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg := getCfg(); cfg != nil && !cfg.Dashboard.IsEnabled() {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// MetricsHandler is implemented by concrete metrics implementations that can
// serve a scrape endpoint. NewRouter degrades to 404 when m cannot serve one.
type MetricsHandler interface {
	Handler() http.Handler
}

// metricsHandler serves the scrape endpoint when metrics are enabled. The
// endpoint is registered unconditionally so enabling metrics in config.yaml
// takes effect without a restart; while disabled it answers 404, matching an
// unregistered path.
func metricsHandler(getCfg func() *config.Config, m Metrics) http.Handler {
	h, ok := m.(MetricsHandler)
	if !ok {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg := getCfg(); cfg == nil || !cfg.Observability.MetricsEnabled() {
			http.NotFound(w, r)
			return
		}
		h.Handler().ServeHTTP(w, r)
	})
}
