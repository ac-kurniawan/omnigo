package api

import (
	"encoding/json"
	"net/http"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type modelEntry struct {
	ID           string                      `json:"id"`
	Object       string                      `json:"object"`
	Created      int                         `json:"created"`
	OwnedBy      string                      `json:"owned_by"`
	Capabilities *provider.ModelCapabilities `json:"capabilities,omitempty"`
}

func handleModels(getCfg func() *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := struct {
			Object string       `json:"object"`
			Data   []modelEntry `json:"data"`
		}{Object: "list", Data: modelEntries(getCfg())}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func handleModel(getCfg func() *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model")
		for _, model := range modelEntries(getCfg()) {
			if model.ID == modelID {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(model)
				return
			}
		}
		writeError(w, http.StatusNotFound, "model not found: "+modelID)
	}
}

func modelEntries(cfg *config.Config) []modelEntry {
	var entries []modelEntry
	for _, cb := range cfg.Combos {
		entry := modelEntry{ID: cb.Name, Object: "model", Created: 0, OwnedBy: "combo"}
		for _, target := range cb.Targets {
			for _, p := range cfg.Providers {
				if p.Name == target.Provider && !p.Disabled && !p.IsModelDisabled(target.Model) && p.HasModel(target.Model) {
					entry.Capabilities = provider.CapabilitiesFor(p.Type, target.Model)
					break
				}
			}
			if entry.Capabilities != nil {
				break
			}
		}
		entries = append(entries, entry)
	}
	for _, p := range cfg.Providers {
		if p.Disabled {
			continue
		}
		for _, m := range p.Models {
			if !p.IsModelDisabled(m) {
				entries = append(entries, modelEntry{ID: p.Name + "/" + m, Object: "model", Created: 0, OwnedBy: p.Name, Capabilities: provider.CapabilitiesFor(p.Type, m)})
			}
		}
	}
	return entries
}
