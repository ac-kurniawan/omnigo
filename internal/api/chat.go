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
)

func handleChat(getCfg func() *config.Config, registry *providerRegistry, tracker *combo.Tracker) http.HandlerFunc {
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
			runCombo(w, r, cfg, registry, cb, req, tracker)
			return
		}

		p, model, ok := resolveDirect(cfg, registry, body.Model)
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

func resolveDirect(cfg *config.Config, registry *providerRegistry, model string) (provider.Provider, string, bool) {
	if i := strings.Index(model, "/"); i > 0 {
		provName := model[:i]
		modelID := model[i+1:]
		for _, pc := range cfg.Providers {
			if pc.Name == provName {
				if pc.Disabled || pc.IsModelDisabled(modelID) {
					return nil, "", false
				}
				p, ok := registry.Get(cfg, pc.Name)
				if !ok {
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
				p, ok := registry.Get(cfg, pc.Name)
				if !ok {
					return nil, "", false
				}
				return p, model, true
			}
		}
	}
	return nil, "", false
}

func runCombo(w http.ResponseWriter, r *http.Request, cfg *config.Config, registry *providerRegistry, cb config.Combo, req provider.ChatRequest, tracker *combo.Tracker) {
	c := combo.Combo{
		Name:     cb.Name,
		Strategy: cb.Strategy,
		Tracker:  tracker,
		DrainTTL: cb.ParsedDrainTTL(),
	}
	for _, t := range cb.Targets {
		c.Targets = append(c.Targets, combo.Target{Provider: t.Provider, Model: t.Model})
	}
	responseCommitted := false
	_, err := c.Run(r.Context(), func(ctx context.Context, t combo.Target) error {
		p, ok := providerForName(cfg, registry, t.Provider)
		if !ok {
			return fmt.Errorf("unknown provider: %s", t.Provider)
		}
		for _, pc := range cfg.Providers {
			if pc.Name == t.Provider && pc.IsModelDisabled(t.Model) {
				return fmt.Errorf("model %q is disabled on provider %q", t.Model, t.Provider)
			}
		}
		req.Model = t.Model
		if cb.Strategy != "reliable" && cb.Strategy != "round-robin" {
			return p.ChatCompletion(ctx, req, w)
		}
		attempt := newBufferedResponseWriter(w, req.Stream)
		providerErr := p.ChatCompletion(ctx, req, attempt)
		responseCommitted = responseCommitted || attempt.committed
		err := attempt.finish(providerErr)
		if errors.Is(err, errResponseCommitted) {
			responseCommitted = true
			if tracker != nil {
				tracker.MarkDrained(t, cb.ParsedDrainTTL(), sanitizeFailure(err))
			}
			return nil
		}
		if err != nil {
			return sanitizedError{err: err, message: sanitizeFailure(err)}
		}
		return nil
	})
	if err != nil && !responseCommitted {
		writeError(w, http.StatusBadGateway, sanitizeFailure(err))
	}
}

type sanitizedError struct {
	err     error
	message string
}

func (e sanitizedError) Error() string {
	return e.message
}

func (e sanitizedError) Unwrap() error {
	return e.err
}

func (e sanitizedError) DrainReason() string {
	return e.message
}

func sanitizeFailure(err error) string {
	if err == nil {
		return "upstream failure"
	}
	fields := strings.Fields(err.Error())
	redactNext := false
	for i, field := range fields {
		trimmed := strings.Trim(field, `"'(),;`)
		lower := strings.ToLower(trimmed)
		if redactNext || strings.HasPrefix(lower, "sk-") || strings.Contains(lower, "token=") || strings.Contains(lower, "api_key=") || strings.Contains(lower, "apikey=") {
			fields[i] = "[redacted]"
			redactNext = false
			continue
		}
		redactNext = lower == "bearer" || strings.HasSuffix(lower, "token:") || strings.HasSuffix(lower, "api_key:") || strings.HasSuffix(lower, "apikey:")
	}
	return strings.Join(fields, " ")
}

func providerForName(cfg *config.Config, registry *providerRegistry, name string) (provider.Provider, bool) {
	for _, pc := range cfg.Providers {
		if pc.Name == name {
			if pc.Disabled {
				return nil, false
			}
			return registry.Get(cfg, name)
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
