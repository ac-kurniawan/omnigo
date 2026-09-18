package api

import (
	"encoding/json"
	"net/http"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/quota"
)

// handleGetQuota serves the full in-memory quota snapshot cache as JSON. It is
// an internal dashboard helper; /v1/* clients do not have access to it.
func handleGetQuota(cache *quota.Cache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cache == nil {
			writeError(w, http.StatusNotFound, "quota cache not enabled")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"snapshots": cache.All(),
		})
	}
}

// handleRefreshQuota performs a synchronous single-credential poll and
// updates the cache in place. It backs the per-account Refresh button in the
// dashboard UI.
func handleRefreshQuota(getCfg func() *config.Config, registry *providerRegistry, cache *quota.Cache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		providerName := r.PathValue("provider")
		identity := r.PathValue("identity")
		if providerName == "" || identity == "" {
			writeError(w, http.StatusBadRequest, "provider and identity are required")
			return
		}
		fetch := registry.quotaFetch(getCfg)
		snapshot, err := fetch(r.Context(), providerName, identity)
		if err != nil && snapshot.Status == "" {
			snapshot.Status = quota.StatusUnavailable
			snapshot.Reason = err.Error()
		}
		if snapshot.Provider == "" {
			snapshot.Provider = providerName
		}
		if snapshot.Identity == "" {
			snapshot.Identity = identity
		}
		if cache != nil {
			cache.Put(snapshot)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot)
	}
}
