package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func handleChat(getCfg func() *config.Config, store *vault.Store, tracker *combo.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		var body struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.Model == "" {
			writeError(w, http.StatusBadRequest, "model is required")
			return
		}
		cfg := getCfg()

		msgs := make([]provider.Message, 0, len(body.Messages))
		for _, m := range body.Messages {
			msgs = append(msgs, provider.Message{Role: m.Role, Content: m.Content})
		}
		req := provider.ChatRequest{Model: body.Model, Stream: body.Stream, Messages: msgs, Raw: raw}

		if cb, ok := findCombo(cfg, body.Model); ok {
			runCombo(w, r, cfg, store, cb, req, tracker)
			return
		}

		p, model, ok := resolveDirect(cfg, store, body.Model)
		if !ok {
			writeError(w, http.StatusNotFound, "model not found: "+body.Model)
			return
		}
		req.Model = model
		if err := p.ChatCompletion(r.Context(), req, w); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
		}
	}
}

func findCombo(cfg *config.Config, name string) (config.Combo, bool) {
	for _, cb := range cfg.Combos {
		if cb.Name == name {
			return cb, true
		}
	}
	return config.Combo{}, false
}

func resolveDirect(cfg *config.Config, store *vault.Store, model string) (provider.Provider, string, bool) {
	if i := strings.Index(model, "/"); i > 0 {
		provName := model[:i]
		modelID := model[i+1:]
		for _, pc := range cfg.Providers {
			if pc.Name == provName {
				if pc.Disabled || pc.IsModelDisabled(modelID) {
					return nil, "", false
				}
				p, err := buildProvider(pc, store, cfg.DefaultTimeout())
				if err != nil {
					return nil, "", false
				}
				return p, modelID, true
			}
		}
	}
	for _, pc := range cfg.Providers {
		if pc.Disabled {
			continue
		}
		for _, m := range pc.Models {
			if m == model {
				if pc.IsModelDisabled(model) {
					return nil, "", false
				}
				p, err := buildProvider(pc, store, cfg.DefaultTimeout())
				if err != nil {
					return nil, "", false
				}
				return p, model, true
			}
		}
	}
	return nil, "", false
}

func runCombo(w http.ResponseWriter, r *http.Request, cfg *config.Config, store *vault.Store, cb config.Combo, req provider.ChatRequest, tracker *combo.Tracker) {
	c := combo.Combo{
		Name:     cb.Name,
		Strategy: cb.Strategy,
		Tracker:  tracker,
		DrainTTL: cb.ParsedDrainTTL(),
	}
	for _, t := range cb.Targets {
		c.Targets = append(c.Targets, combo.Target{Provider: t.Provider, Model: t.Model})
	}
	_, err := c.Run(r.Context(), func(ctx context.Context, t combo.Target) error {
		p, ok := providerForName(cfg, store, t.Provider)
		if !ok {
			return fmt.Errorf("unknown provider: %s", t.Provider)
		}
		for _, pc := range cfg.Providers {
			if pc.Name == t.Provider && pc.IsModelDisabled(t.Model) {
				return fmt.Errorf("model %q is disabled on provider %q", t.Model, t.Provider)
			}
		}
		req.Model = t.Model
		return p.ChatCompletion(ctx, req, w)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
	}
}

func providerForName(cfg *config.Config, store *vault.Store, name string) (provider.Provider, bool) {
	for _, pc := range cfg.Providers {
		if pc.Name == name {
			if pc.Disabled {
				return nil, false
			}
			p, err := buildProvider(pc, store, cfg.DefaultTimeout())
			if err != nil {
				return nil, false
			}
			return p, true
		}
	}
	return nil, false
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": statusToCode(status)},
	})
}

func statusToCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid_api_key"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusBadGateway:
		return "upstream_error"
	default:
		return "invalid_request_error"
	}
}
