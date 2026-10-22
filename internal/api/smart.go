package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/jev"
	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// smartRouter decides which route a request should take and resolves that
// route to the target OmniGo dispatches to.
//
// The decision is fail-open: when Jev is unreachable, slow, unconfident, or
// answers with something that is not a route, the combo's default route is
// used instead. The router never rejects a request on its own.
type smartRouter struct {
	cfg   *config.Config
	jev   jevDecider
	combo config.Combo
}

// jevDecider is the part of the jev.Client the router uses. It is an
// interface so the routing logic can be tested without a network call.
type jevDecider interface {
	Decide(ctx context.Context, state string, routes []jev.Route) (jev.Decision, error)
}

// newSmartRouter builds a router for a smart combo. The decider may be nil,
// which makes every decision fail and therefore always take the default route.
func newSmartRouter(cfg *config.Config, cb config.Combo, decider jevDecider) *smartRouter {
	return &smartRouter{cfg: cfg, jev: decider, combo: cb}
}

// smartRoutes projects the combo's routes into the form Jev is asked about.
// Only the name and the criteria text are sent: the destination is OmniGo's
// business, not Jev's.
func (r *smartRouter) smartRoutes() []jev.Route {
	routes := make([]jev.Route, 0, len(r.combo.Routes))
	for _, route := range r.combo.Routes {
		routes = append(routes, jev.Route{Name: route.Name, Criteria: route.Criteria})
	}
	return routes
}

// smartState is the text Jev judges: the last user message. It is the
// cheapest signal that still describes the current task, and it keeps the
// billed input small.
func smartState(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != "user" && msg.Role != "tool" && msg.Role != "function" {
			continue
		}
		if text := messageText(msg.Content); text != "" {
			return text
		}
	}
	// No user-role message carried text. Fall back to the last message of any
	// role so a tool-only turn still gets a state.
	for i := len(msgs) - 1; i >= 0; i-- {
		if text := messageText(msgs[i].Content); text != "" {
			return text
		}
	}
	return ""
}

// messageText renders a message's content as plain text. String content is
// used as-is; anything else (a multimodal array, an object) is JSON-encoded so
// Jev still receives the information as text.
func messageText(content any) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if text := messageText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprintf("%v", value)
		}
		return string(raw)
	}
}

// smartDecision is the outcome of one routing decision.
type smartDecision struct {
	Route      string
	Confidence float64
	// Fallback is true when the default route was used because the decision
	// did not succeed.
	Fallback bool
	// Reason explains a fallback, for the log and the metrics label.
	Reason string
}

// decide asks Jev which route applies. Any failure, including a low-confidence
// or unknown answer, falls back to the combo's default route.
func (r *smartRouter) decide(ctx context.Context, state string) smartDecision {
	routes := r.smartRoutes()
	if len(routes) == 0 {
		return smartDecision{Route: r.combo.DefaultRoute, Fallback: true, Reason: "no routes configured"}
	}
	if r.jev == nil {
		return smartDecision{Route: r.combo.DefaultRoute, Fallback: true, Reason: "jev client unavailable"}
	}
	decision, err := r.jev.Decide(ctx, state, routes)
	if err != nil {
		reason := err.Error()
		// The error text may quote upstream content, so keep it short.
		if len(reason) > 160 {
			reason = reason[:160] + "…"
		}
		return smartDecision{Route: r.combo.DefaultRoute, Fallback: true, Reason: reason}
	}
	return smartDecision{Route: decision.Route, Confidence: decision.Confidence}
}

// resolve turns a route name into the provider and model to dispatch to.
//
// A route's destination is either a combo name or a provider/model pair. A
// combo destination is expanded to that combo's own chain, so the destination
// combo — not the smart combo — owns the failover. A plain combo is used with
// its configured strategy, which is why the whole chain is returned rather
// than just its first target.
func (r *smartRouter) resolve(routeName string) ([]comboTarget, bool) {
	for _, route := range r.combo.Routes {
		if route.Name != routeName {
			continue
		}
		return r.resolveDestination(route.Destination)
	}
	return nil, false
}

// resolveDestination expands a destination into the targets to try, in order.
func (r *smartRouter) resolveDestination(destination string) ([]comboTarget, bool) {
	if target, ok := config.FindCombo(r.cfg, destination); ok {
		targets := make([]comboTarget, 0, len(target.Targets))
		for _, t := range target.Targets {
			targets = append(targets, comboTarget{Provider: t.Provider, Model: t.Model})
		}
		if len(targets) == 0 {
			return nil, false
		}
		return targets, true
	}
	providerName, model, ok := strings.Cut(destination, "/")
	if !ok || providerName == "" || model == "" {
		return nil, false
	}
	return []comboTarget{{Provider: providerName, Model: model}}, true
}

// comboTarget is a provider/model pair to dispatch to.
type comboTarget struct {
	Provider string
	Model    string
}

// logDecision records a routing decision. It never prints the request text.
func logDecision(comboName string, decision smartDecision) {
	if decision.Fallback {
		log.Printf("smart combo %q: using default route %q (%s)", comboName, decision.Route, decision.Reason)
		return
	}
	log.Printf("smart combo %q: route %q (confidence %.2f)", comboName, decision.Route, decision.Confidence)
}
