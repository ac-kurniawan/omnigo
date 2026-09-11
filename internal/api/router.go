package api

import (
	"net/http"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func NewRouter(getCfg func() *config.Config, store *vault.Store, mutate config.MutateFunc, tracker *combo.Tracker) http.Handler {
	mux := http.NewServeMux()

	// /v1/* is gated by client API keys.
	mux.Handle("POST /v1/chat/completions", auth.Middleware(store.Get)(http.HandlerFunc(handleChat(getCfg, store, tracker))))
	mux.Handle("GET /v1/models", auth.Middleware(store.Get)(http.HandlerFunc(handleModels(getCfg))))

	mux.Handle("POST /internal/refresh-models/{provider}", auth.Middleware(store.Get)(http.HandlerFunc(handleRefreshModels(getCfg, store, mutate))))
	mux.Handle("POST /internal/test/{provider}", auth.Middleware(store.Get)(http.HandlerFunc(handleTestProvider(getCfg, store))))

	return mux
}
