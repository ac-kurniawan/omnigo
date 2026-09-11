package api

import (
	"encoding/json"
	"net/http"

	"github.com/ac-kurniawan/omnigo/internal/config"
)

func handleModels(getCfg func() *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := getCfg()
		type modelEntry struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int    `json:"created"`
			OwnedBy string `json:"owned_by"`
		}
		out := struct {
			Object string       `json:"object"`
			Data   []modelEntry `json:"data"`
		}{Object: "list"}
		for _, cb := range cfg.Combos {
			out.Data = append(out.Data, modelEntry{ID: cb.Name, Object: "model", Created: 0, OwnedBy: "combo"})
		}
		for _, p := range cfg.Providers {
			if p.Disabled {
				continue
			}
			for _, m := range p.Models {
				out.Data = append(out.Data, modelEntry{ID: p.Name + "/" + m, Object: "model", Created: 0, OwnedBy: p.Name})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}
