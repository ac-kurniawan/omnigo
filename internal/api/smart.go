package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/systemone"
)

// smartRouter decides which route a request should take and resolves that
// route to the target OmniGo dispatches to.
//
// The decision is fail-open: when the System One model is unreachable, slow,
// unconfident, or answers with something that is not a route, the combo's
// default route is used instead. The router never rejects a request on its own.
type smartRouter struct {
	cfg     *config.Config
	decider smartDecider
	combo   config.Combo
}

// smartDecider is the part of the systemone.Client the router uses. It is an
// interface so the routing logic can be tested without a network call, and so
// the router is not tied to one decision model.
type smartDecider interface {
	// Decide picks a route for state. The returned route is expected to be
	// one of routes; the caller checks that before using it.
	Decide(ctx context.Context, state string, routes []config.SmartRoute) (smartVerdict, error)
}

// smartVerdict is a decider's answer.
type smartVerdict struct {
	// Route is the chosen route name.
	Route string
	// Confidence is how sure the decider is, 0..1.
	Confidence float64
}

// newSmartRouter builds a router for a smart combo. The decider may be nil,
// which makes every decision fail and therefore always take the default route.
func newSmartRouter(cfg *config.Config, cb config.Combo, decider smartDecider) *smartRouter {
	return &smartRouter{cfg: cfg, decider: decider, combo: cb}
}

// smartState is the text the decision model judges: the last user message. It
// is the cheapest signal that still describes the current task, and it keeps
// the billed input small.
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
// the decision model still receives the information as text.
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

// decide asks the decision model which route applies. Any failure, including a
// low-confidence or unknown answer, falls back to the combo's default route.
func (r *smartRouter) decide(ctx context.Context, state string) smartDecision {
	routes := r.combo.Routes
	if len(routes) == 0 {
		return smartDecision{Route: r.combo.DefaultRoute, Fallback: true, Reason: "no routes configured"}
	}
	if r.decider == nil {
		return smartDecision{Route: r.combo.DefaultRoute, Fallback: true, Reason: "decision client unavailable"}
	}
	verdict, err := r.decider.Decide(ctx, state, routes)
	if err != nil {
		reason := err.Error()
		// The error text may quote upstream content, so keep it short.
		if len(reason) > 160 {
			reason = reason[:160] + "…"
		}
		return smartDecision{Route: r.combo.DefaultRoute, Fallback: true, Reason: reason}
	}
	return smartDecision{Route: verdict.Route, Confidence: verdict.Confidence}
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

// systemOneDecider asks a System One model which route fits. It supports two
// ways of putting the question, chosen by the combo's decision mode.
type systemOneDecider struct {
	client      *systemone.Client
	mode        string
	question    string
	minConf     float64
	weights     map[string]float64
	questionIDs map[string]string
}

// newSystemOneDecider builds a decider backed by a System One model.
func newSystemOneDecider(client *systemone.Client, decision config.SmartDecision) *systemOneDecider {
	return &systemOneDecider{
		client:   client,
		mode:     decision.ModeOrDefault(),
		question: decision.QuestionOrDefault(),
		minConf:  decision.ParsedMinConfidence(),
		weights:  decision.Weights,
	}
}

// Decide implements smartDecider.
func (d *systemOneDecider) Decide(ctx context.Context, state string, routes []config.SmartRoute) (smartVerdict, error) {
	if d.client == nil {
		return smartVerdict{}, systemone.ErrNoAPIKey
	}
	if d.mode == config.DecisionModeFanout {
		return d.decideFanout(ctx, state, routes)
	}
	return d.decideChoice(ctx, state, routes)
}

// decideChoice asks one choice question whose options are the routes and takes
// the selected option.
func (d *systemOneDecider) decideChoice(ctx context.Context, state string, routes []config.SmartRoute) (smartVerdict, error) {
	criteria := make(map[string]string, len(routes))
	for _, route := range routes {
		criteria[route.Name] = route.Criteria
	}
	answers, err := d.client.Ask(ctx, state, []systemone.Question{{
		ID:           smartQuestionID,
		Type:         systemone.TypeChoice,
		Instructions: d.question,
		Criteria:     criteria,
	}})
	if err != nil {
		return smartVerdict{}, err
	}
	answer, ok := answers[smartQuestionID]
	if !ok {
		return smartVerdict{}, fmt.Errorf("%w: no answer for question %q", systemone.ErrInvalidResponse, smartQuestionID)
	}
	if answer.Choice == "" {
		return smartVerdict{}, fmt.Errorf("%w: empty choice", systemone.ErrInvalidResponse)
	}
	if answer.Confidence < d.minConf {
		return smartVerdict{}, fmt.Errorf("%w: confidence %.2f below floor %.2f", systemone.ErrLowConfidence, answer.Confidence, d.minConf)
	}
	return smartVerdict{Route: answer.Choice, Confidence: answer.Confidence}, nil
}

// decideFanout asks one noul question per route and picks the route with the
// highest weighted probability. Every question is sent in a single request, so
// the cost is one call, and the weights let a route's importance be tuned
// without rewording anything.
func (d *systemOneDecider) decideFanout(ctx context.Context, state string, routes []config.SmartRoute) (smartVerdict, error) {
	// Sorting keeps the question order stable regardless of how the routes are
	// configured, so the same request always asks the same questions.
	ordered := sortRoutesByName(routes)
	questions := make([]systemone.Question, 0, len(ordered))
	byID := make(map[string]string, len(ordered))
	for _, route := range ordered {
		id := smartQuestionID + "_" + route.Name
		questions = append(questions, systemone.Question{
			ID:           id,
			Type:         systemone.TypeNoul,
			Instructions: fanoutInstruction(route, d.question),
		})
		byID[id] = route.Name
	}
	answers, err := d.client.Ask(ctx, state, questions)
	if err != nil {
		return smartVerdict{}, err
	}

	best := ""
	bestScore := -1.0
	for _, question := range questions {
		answer, ok := answers[question.ID]
		if !ok {
			return smartVerdict{}, fmt.Errorf("%w: no answer for question %q", systemone.ErrInvalidResponse, question.ID)
		}
		routeName := byID[question.ID]
		score := answer.Noul * d.weightFor(routeName)
		if score > bestScore {
			bestScore = score
			best = routeName
		}
	}
	if best == "" {
		return smartVerdict{}, fmt.Errorf("%w: no route scored", systemone.ErrInvalidResponse)
	}
	// A noul answer carries no confidence of its own, so the winning weighted
	// probability stands in for it: it is the model's own probability for
	// that route, scaled by how much the route is trusted.
	if bestScore < d.minConf {
		return smartVerdict{}, fmt.Errorf("%w: score %.2f below floor %.2f", systemone.ErrLowConfidence, bestScore, d.minConf)
	}
	return smartVerdict{Route: best, Confidence: bestScore}, nil
}

// weightFor returns the configured weight for a route, or 1.
func (d *systemOneDecider) weightFor(routeName string) float64 {
	weight, ok := d.weights[routeName]
	if !ok || weight <= 0 {
		return 1
	}
	return weight
}

// smartQuestionID is the key a choice answer comes back under. It is fixed so
// the response parse does not depend on the caller's wording.
const smartQuestionID = "route"

// fanoutInstruction builds the noul question for one route.
func fanoutInstruction(route config.SmartRoute, base string) string {
	instruction := "Is this request best handled by the " + route.Name + " route?"
	if route.Criteria != "" {
		instruction += " That route is for: " + route.Criteria
	}
	if base != "" {
		instruction += " Context: " + base
	}
	return instruction
}

// sortRoutesByName returns the routes sorted by name, so a deterministic
// question order is used regardless of configuration order.
func sortRoutesByName(routes []config.SmartRoute) []config.SmartRoute {
	sorted := make([]config.SmartRoute, len(routes))
	copy(sorted, routes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	return sorted
}
