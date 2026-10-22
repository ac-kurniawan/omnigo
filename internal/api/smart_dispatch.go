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
	"github.com/ac-kurniawan/omnigo/internal/jev"
	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// jevClientFactory builds a Jev client for one smart combo. The API key comes
// from the environment (TYPESAFE_API_KEY), so a client is only usable when that
// variable is set; a combo without it always takes its default route.
type jevClientFactory func(cfg config.SmartJev) jevDecider

// jevClientFactoryOverride replaces the environment-backed factory. Tests set
// it to point the client at a local stand-in server.
var jevClientFactoryOverride jevClientFactory

// jevClientFactoryEnv reads the key from the environment.
func jevClientFactoryEnv(cfg config.SmartJev) jevDecider {
	return jev.New(jev.Config{
		Model:         cfg.ModelOrDefault(),
		APIKey:        os.Getenv("TYPESAFE_API_KEY"),
		Timeout:       cfg.ParsedTimeout(),
		MinConfidence: cfg.ParsedMinConfidence(),
		Question:      cfg.QuestionOrDefault(),
	})
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

// smartDeciderFor returns the Jev decider for a smart combo. The client is
// cached per combo configuration so a burst of requests reuses one HTTP
// client, and it is rebuilt when the combo's Jev settings change.
var smartJevClients sync.Map

func smartDeciderFor(cfg *config.Config, cb config.Combo) jevDecider {
	if jevClientFactoryOverride != nil {
		return jevClientFactoryOverride(cb.Jev)
	}
	key := smartClientKey(cb)
	if cached, ok := smartJevClients.Load(key); ok {
		if client, ok := cached.(jevDecider); ok {
			return client
		}
	}
	client := jevClientFactoryEnv(cb.Jev)
	smartJevClients.Store(key, client)
	return client
}

// smartClientKey identifies a Jev client by the settings that shape it.
func smartClientKey(cb config.Combo) string {
	return cb.Name + "\x00" + cb.Jev.ModelOrDefault() + "\x00" + cb.Jev.ParsedTimeout().String() + "\x00" + fmt.Sprintf("%g", cb.Jev.ParsedMinConfidence()) + "\x00" + cb.Jev.QuestionOrDefault()
}
