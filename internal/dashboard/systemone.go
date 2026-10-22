package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/systemone"
)

// systemOneModelsTimeout bounds the model-list call so a slow upstream cannot
// hold a dashboard request open.
const systemOneModelsTimeout = 10 * time.Second

// systemOneModelsBaseURL overrides the endpoint the model list is read from.
// Tests point it at a local stand-in server.
var systemOneModelsBaseURL string

// getSystemOneModels lists the System One models the account can use.
//
// Every System One model is served by the same endpoint, and the request's
// model field picks the handler, so an operator can switch a smart combo to
// any model the account has access to without editing the config by hand.
func (s *Server) getSystemOneModels(w http.ResponseWriter, r *http.Request) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		http.Error(w, "TYPESAFE_API_KEY is not set: no System One models can be listed", http.StatusServiceUnavailable)
		return
	}

	client := systemone.New(systemone.Config{
		BaseURL: systemOneModelsBaseURL,
		APIKey:  key,
		Timeout: systemOneModelsTimeout,
	})
	models, err := client.ListModels(r.Context())
	if err != nil {
		http.Error(w, "could not list System One models: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"models": models}); err != nil {
		// The status line is already written, so nothing else can be said.
		return
	}
}
