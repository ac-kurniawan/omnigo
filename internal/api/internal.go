package api

import (
	"encoding/json"
	"net/http"

	"github.com/ac-kurniawan/omnigo/internal/config"
)

func handleRefreshModels(getCfg func() *config.Config, registry *providerRegistry, mutate config.MutateFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("provider")
		cfg := getCfg()
		for _, pc := range getCfg().Providers {
			if pc.Name != name {
				continue
			}
			p, ok := registry.Get(cfg, pc.Name)
			if !ok {
				writeError(w, http.StatusInternalServerError, "provider unavailable: "+pc.Type)
				return
			}
			models, err := p.Models(r.Context())
			if err != nil {
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
			ids := make([]string, 0, len(models))
			for _, m := range models {
				ids = append(ids, m.ID)
			}
			if mutate != nil {
				if err := mutate(func(c *config.Config) error { return config.SetModels(c, name, ids) }); err != nil {
					writeError(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"provider": name, "models": models})
			return
		}
		writeError(w, http.StatusNotFound, "provider not found: "+name)
	}
}

func handleTestProvider(getCfg func() *config.Config, registry *providerRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("provider")
		cfg := getCfg()
		for _, pc := range getCfg().Providers {
			if pc.Name != name {
				continue
			}
			p, ok := registry.Get(cfg, pc.Name)
			if !ok {
				writeError(w, http.StatusInternalServerError, "provider unavailable: "+pc.Type)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(p.Test(r.Context()))
			return
		}
		writeError(w, http.StatusNotFound, "provider not found: "+name)
	}
}
