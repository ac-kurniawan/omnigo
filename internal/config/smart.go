package config

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Decision modes. A smart combo asks a System One model which route fits; the
// mode decides how the question is put.
const (
	// DecisionModeChoice asks one choice question whose options are the
	// routes. One call, one answer, cheapest.
	DecisionModeChoice = "choice"
	// DecisionModeFanout asks one noul question per route and picks the
	// highest weighted probability. More calls' worth of judgment in one
	// request, and it lets a route's importance be tuned without rewording
	// the question.
	DecisionModeFanout = "fanout"
)

// SmartDecision configures the System One call a smart combo makes before it
// dispatches. Every field is optional; the zero value is a working
// configuration that matches the systemone package's defaults.
type SmartDecision struct {
	// Model is the System One model id or alias to ask, e.g. jev-latest.
	// System One is a class of models, not a single one, so this is a plain
	// string: any model the account can call works.
	Model string `yaml:"model,omitempty"`
	// BaseURL overrides the API endpoint. Empty means the public TypeSafe API.
	BaseURL string `yaml:"base_url,omitempty"`
	// Timeout bounds the decision call. Default: 4s. A decision that overruns
	// is abandoned and the combo falls back to its default route.
	Timeout string `yaml:"timeout,omitempty"`
	// MinConfidence is the floor below which the answer is not trusted and
	// the default route is used instead. Default: 0.5.
	MinConfidence string `yaml:"min_confidence,omitempty"`
	// Question is the instruction for a choice-mode decision. Default: a
	// generic "which model should handle this request?" prompt.
	Question string `yaml:"question,omitempty"`
	// Mode is choice or fanout. Default: choice.
	Mode string `yaml:"mode,omitempty"`
	// Weights scales each route's fanout probability. A missing route weighs
	// 1.0. Ignored in choice mode.
	Weights map[string]float64 `yaml:"weights,omitempty"`
}

// ParsedTimeout returns the decision budget, falling back to 4s.
func (d SmartDecision) ParsedTimeout() time.Duration {
	if d.Timeout == "" {
		return 4 * time.Second
	}
	parsed, err := time.ParseDuration(d.Timeout)
	if err != nil || parsed <= 0 {
		return 4 * time.Second
	}
	return parsed
}

// ParsedMinConfidence returns the confidence floor, falling back to 0.5.
func (d SmartDecision) ParsedMinConfidence() float64 {
	if d.MinConfidence == "" {
		return 0.5
	}
	var parsed float64
	if _, err := fmt.Sscanf(strings.TrimSpace(d.MinConfidence), "%g", &parsed); err != nil {
		return 0.5
	}
	if parsed < 0 || parsed > 1 {
		return 0.5
	}
	return parsed
}

// ModelOrDefault returns the System One model to call.
func (d SmartDecision) ModelOrDefault() string {
	if strings.TrimSpace(d.Model) == "" {
		return "jev-latest"
	}
	return strings.TrimSpace(d.Model)
}

// BaseURLOrDefault returns the API endpoint to call. Empty means the public
// TypeSafe API, which serves every System One model.
func (d SmartDecision) BaseURLOrDefault() string {
	return strings.TrimSpace(d.BaseURL)
}

// WeightsString renders the weights as "route=weight, route=weight" in a
// stable order, which is what the dashboard's text field round-trips.
func (d SmartDecision) WeightsString() string {
	if len(d.Weights) == 0 {
		return ""
	}
	names := make([]string, 0, len(d.Weights))
	for name := range d.Weights {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%g", name, d.Weights[name]))
	}
	return strings.Join(parts, ", ")
}

// QuestionOrDefault returns the instruction a choice-mode decision answers.
func (d SmartDecision) QuestionOrDefault() string {
	if strings.TrimSpace(d.Question) == "" {
		return "Which model should handle this request?"
	}
	return strings.TrimSpace(d.Question)
}

// ModeOrDefault returns the decision mode, falling back to choice.
func (d SmartDecision) ModeOrDefault() string {
	switch strings.TrimSpace(d.Mode) {
	case DecisionModeFanout:
		return DecisionModeFanout
	default:
		return DecisionModeChoice
	}
}

// WeightFor returns the configured weight for a route, or 1.0.
func (d SmartDecision) WeightFor(routeName string) float64 {
	weight, ok := d.Weights[routeName]
	if !ok || weight <= 0 {
		return 1
	}
	return weight
}

// SmartRoute is one option in a smart combo. The decision model chooses
// between routes by reading Criteria; Destination is where OmniGo sends the
// request once the route wins. It is never sent to the model.
type SmartRoute struct {
	Name     string `yaml:"name" json:"name"`
	Criteria string `yaml:"criteria" json:"criteria"`
	// Destination is either an existing combo name (combo/flash) or a
	// "provider/model" pair (agy/gemini-3.8-flash). Combo names are matched
	// first, so a name that collides with a provider is treated as a combo.
	Destination string `yaml:"destination" json:"destination"`
}

// DestinationIsCombo reports whether this route's destination names one of the
// given combos. Combo names and provider/model pairs both contain slashes, so
// membership — not string shape — decides.
func (r SmartRoute) DestinationIsCombo(combos []Combo) bool {
	for _, cb := range combos {
		if cb.Name == r.Destination {
			return true
		}
	}
	return false
}

// FindCombo returns the combo with the given exact name. It is the single
// resolution rule for both client requests and smart-route destinations.
func FindCombo(cfg *Config, name string) (Combo, bool) {
	if cfg == nil {
		return Combo{}, false
	}
	for _, cb := range cfg.Combos {
		if cb.Name == name {
			return cb, true
		}
	}
	return Combo{}, false
}

// IsSmart reports whether this combo routes through a System One model.
func (c Combo) IsSmart() bool { return c.Strategy == StrategySmart }

// StrategySmart is the System One-backed routing strategy.
const StrategySmart = "smart"

// validateSmart checks the smart-combo invariants: a non-empty route set with
// unique names, a resolvable destination per route, a default route that exists,
// and one level of nesting only.
func (c Combo) validateSmart(cfg *Config, providers map[string]Provider) error {
	if len(c.Targets) > 0 {
		return fmt.Errorf("combo %q: a smart combo must not declare targets; put the chain in the destination combo", c.Name)
	}
	if len(c.Routes) == 0 {
		return fmt.Errorf("combo %q: at least one route is required for the smart strategy", c.Name)
	}
	if err := validateSmartDecisionFields(c.Name, c.Decision); err != nil {
		return err
	}

	seen := map[string]bool{}
	for _, route := range c.Routes {
		if strings.TrimSpace(route.Name) == "" {
			return fmt.Errorf("combo %q: route name is required", c.Name)
		}
		if seen[route.Name] {
			return fmt.Errorf("combo %q: duplicate route %q", c.Name, route.Name)
		}
		seen[route.Name] = true
		if strings.TrimSpace(route.Criteria) == "" {
			return fmt.Errorf("combo %q: route %q: criteria is required", c.Name, route.Name)
		}
		if strings.TrimSpace(route.Destination) == "" {
			return fmt.Errorf("combo %q: route %q: destination is required", c.Name, route.Name)
		}
		if err := c.validateDestination(cfg, route, providers); err != nil {
			return err
		}
	}
	if c.DefaultRoute == "" {
		return fmt.Errorf("combo %q: default_route is required for the smart strategy", c.Name)
	}
	if !seen[c.DefaultRoute] {
		return fmt.Errorf("combo %q: default_route %q is not one of its routes", c.Name, c.DefaultRoute)
	}
	return nil
}

// validateDestination resolves one route's destination: an existing plain combo,
// or a provider/model pair. A smart combo is rejected because routing through
// it would recurse.
func (c Combo) validateDestination(cfg *Config, route SmartRoute, providers map[string]Provider) error {
	if route.Destination == c.Name {
		return fmt.Errorf("combo %q: route %q cannot target the combo itself", c.Name, route.Name)
	}
	// A destination naming a combo is a combo. Combo names and provider/model
	// pairs both contain slashes, so membership decides, not string shape.
	if target, ok := FindCombo(cfg, route.Destination); ok {
		if target.IsSmart() {
			return fmt.Errorf("combo %q: route %q targets smart combo %q; smart routes may only target a plain combo (one level)", c.Name, route.Name, target.Name)
		}
		return nil
	}
	providerName, model, ok := strings.Cut(route.Destination, "/")
	if !ok || providerName == "" || model == "" {
		return fmt.Errorf("combo %q: route %q: unknown destination %q (want a combo name or provider/model)", c.Name, route.Name, route.Destination)
	}
	pc, ok := providers[providerName]
	if !ok {
		return fmt.Errorf("combo %q: route %q: unknown destination provider %q", c.Name, route.Name, providerName)
	}
	catalog := pc.catalogFor()
	if len(catalog) > 0 && !listedTarget(pc.Type, catalog, model) {
		return fmt.Errorf("combo %q: route %q: provider %q has no model %q", c.Name, route.Name, providerName, model)
	}
	return nil
}
