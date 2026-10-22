package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/systemone"
)

// systemOneClientFactory builds a System One client for one smart combo. The
// API key comes from the environment (TYPESAFE_API_KEY), so a client is only
// usable when that variable is set; a combo without it always takes its
// default route.
type systemOneClientFactory func(cfg config.SmartDecision) smartDecider

// systemOneClientFactoryOverride replaces the environment-backed factory.
// Tests set it to point the client at a local stand-in server.
var systemOneClientFactoryOverride systemOneClientFactory

// systemOneClientFactoryEnv reads the key from the environment.
func systemOneClientFactoryEnv(cfg config.SmartDecision) smartDecider {
	client := systemone.New(systemone.Config{
		Model:   cfg.ModelOrDefault(),
		BaseURL: cfg.BaseURL,
		APIKey:  os.Getenv("TYPESAFE_API_KEY"),
		Timeout: cfg.ParsedTimeout(),
	})
	return newSystemOneDecider(client, cfg)
}

// smartDispatch runs the request against the targets a smart route resolved.
//
// The destination combo owns the failover: the targets are the destination
// combo's own chain, run with the destination combo's strategy. A smart combo
// contributes only the choice of chain.
func runSmartCombo(w http.ResponseWriter, r *http.Request, cfg *config.Config, registry *providerRegistry, cb config.Combo, req provider.ChatRequest, tracker *combo.Tracker, tc *TraceContext, startTime time.Time, metrics Metrics) {
	router := newSmartRouter(cfg, cb, smartDeciderFor(cfg, cb))
	state := smartState(req.Messages)
	decision := router.decide(r.Context(), state)
	logDecision(cb.Name, decision)
	decisionResult := resultSuccess
	if decision.Fallback {
		decisionResult = "fallback"
	}
	metrics.RecordSmartDecision(r.Context(), cb.Name, decision.Route, decisionResult)

	targets, ok := router.resolve(decision.Route)
	if !ok {
		// The route resolved to nothing usable. Fall back to the default
		// route, and if that is also unusable, fail the request.
		if decision.Route != cb.DefaultRoute {
			if fallbackTargets, fallbackOK := router.resolve(cb.DefaultRoute); fallbackOK {
				targets = fallbackTargets
				ok = true
				decision.Fallback = true
				decision.Reason = "route destination did not resolve"
				logDecision(cb.Name, decision)
			}
		}
		if !ok {
			metrics.RecordCombinationAttempt(r.Context(), cb.Name, resultFailure)
			writeError(w, http.StatusBadGateway, "smart combo: no usable destination for route "+decision.Route)
			return
		}
	}

	// The destination combo's strategy governs the failover, so the chain runs
	// under the destination's name. A route pointing straight at a
	// provider/model has no strategy of its own and runs as a priority chain.
	strategy := "priority"
	comboName := cb.Name
	if destination, found := config.FindCombo(cfg, destinationNameFor(cb, decision.Route)); found {
		strategy = destination.Strategy
		comboName = destination.Name
	}

	c := combo.Combo{
		Name:     comboName,
		Strategy: strategy,
		Tracker:  tracker,
		DrainTTL: cb.ParsedDrainTTL(),
		Timeout:  cb.ParsedTimeout(),
	}
	for _, t := range targets {
		c.Targets = append(c.Targets, combo.Target{Provider: t.Provider, Model: t.Model})
	}

	responseCommitted := false
	_, err := c.Run(r.Context(), func(ctx context.Context, t combo.Target) error {
		p, ok := providerForName(cfg, registry, t.Provider)
		if !ok {
			metrics.RecordProviderRequest(ctx, t.Provider, t.Model, resultUnavailable)
			return fmt.Errorf("unknown provider: %s", t.Provider)
		}
		for _, pc := range cfg.Providers {
			if pc.Name == t.Provider && pc.IsModelDisabled(t.Model) {
				metrics.RecordProviderRequest(ctx, t.Provider, t.Model, resultUnavailable)
				return fmt.Errorf("model %q is disabled on provider %q", t.Model, t.Provider)
			}
		}
		req.Model = t.Model
		attempt := newBufferedResponseWriter(w, req.Stream)
		SetTelemetryHeaders(attempt, tc, t.Provider, t.Model, startTime)
		providerCtx := withTokenUsage(ctx, metrics, t.Provider, t.Model, comboName)
		providerErr := p.ChatCompletion(providerCtx, req, attempt)
		err := attempt.finish(providerErr)
		if errors.Is(err, errResponseCommitted) {
			responseCommitted = true
			clientGone := r.Context().Err() != nil
			gatewayDeadline := ctx.Err() != nil && !clientGone
			metrics.RecordProviderRequest(ctx, t.Provider, t.Model, classifyProviderError(err, true, clientGone))
			if tracker != nil && strategy != "priority" && !clientGone && !gatewayDeadline {
				tracker.MarkDrained(t, cb.ParsedDrainTTL(), sanitizeFailure(err))
			}
			if req.Stream && !clientGone {
				writeStreamError(w, sanitizeFailure(err))
			}
			return nil
		}
		if err != nil {
			metrics.RecordProviderRequest(ctx, t.Provider, t.Model, classifyProviderError(err, false, false))
			return sanitizedError{err: err, message: sanitizeFailure(err)}
		}
		metrics.RecordProviderRequest(ctx, t.Provider, t.Model, resultSuccess)
		return nil
	})
	attemptResult := resultSuccess
	if err != nil {
		attemptResult = resultFailure
	}
	metrics.RecordCombinationAttempt(r.Context(), cb.Name, attemptResult)
	if err != nil && !responseCommitted {
		writeProviderError(w, err)
	}
}

// destinationNameFor returns the destination string of the named route.
func destinationNameFor(cb config.Combo, routeName string) string {
	for _, route := range cb.Routes {
		if route.Name == routeName {
			return route.Destination
		}
	}
	return ""
}

// smartDeciderFor returns the decider for a smart combo. The client is cached
// per combo configuration so a burst of requests reuses one HTTP client, and it
// is rebuilt when the combo's decision settings change.
var smartSystemOneClients sync.Map

func smartDeciderFor(cfg *config.Config, cb config.Combo) smartDecider {
	if systemOneClientFactoryOverride != nil {
		return systemOneClientFactoryOverride(cb.Decision)
	}
	key := smartClientKey(cb)
	if cached, ok := smartSystemOneClients.Load(key); ok {
		if client, ok := cached.(smartDecider); ok {
			return client
		}
	}
	client := systemOneClientFactoryEnv(cb.Decision)
	smartSystemOneClients.Store(key, client)
	return client
}

// smartClientKey identifies a System One client by the settings that shape it.
func smartClientKey(cb config.Combo) string {
	return cb.Name + "\x00" + cb.Decision.ModelOrDefault() + "\x00" + cb.Decision.BaseURL + "\x00" +
		cb.Decision.ParsedTimeout().String() + "\x00" + fmt.Sprintf("%g", cb.Decision.ParsedMinConfidence()) + "\x00" +
		cb.Decision.QuestionOrDefault() + "\x00" + cb.Decision.ModeOrDefault()
}
