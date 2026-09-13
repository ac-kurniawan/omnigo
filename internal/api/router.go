package api

import (
	"net/http"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func NewRouter(getCfg func() *config.Config, store *vault.Store, mutate config.MutateFunc, tracker *combo.Tracker, version string) http.Handler {
	registry := newProviderRegistry(store)
	registry.ensure(getCfg())
	mux := http.NewServeMux()
	authed := auth.Middleware(store.Get)

	mux.HandleFunc("GET /health", handleHealth(version))

	// /v1/* is gated by client API keys.
	mux.Handle("POST /v1/chat/completions", authed(http.HandlerFunc(handleChat(getCfg, registry, tracker))))
	mux.Handle("GET /v1/models", authed(http.HandlerFunc(handleModels(getCfg))))
	mux.Handle("GET /v1/models/{model...}", authed(http.HandlerFunc(handleModel(getCfg))))

	mux.Handle("POST /internal/refresh-models/{provider}", authed(http.HandlerFunc(handleRefreshModels(getCfg, registry, mutate))))
	mux.Handle("POST /internal/test/{provider}", authed(http.HandlerFunc(handleTestProvider(getCfg, registry))))

	return mux
}
