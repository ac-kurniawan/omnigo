package config

import (
	"fmt"
	"strings"
	"time"
)

// SmartJev configures the Jev (TypeSafe System One) decision call a smart combo
// makes before it dispatches. Every field is optional; the zero value is a
// working configuration that matches the jev package's defaults.
type SmartJev struct {
	// Model is the Jev model or alias to ask. Default: jev-latest.
	Model string `yaml:"model,omitempty"`
	// Timeout bounds the decision call. Default: 4s. A decision that overruns
	// is abandoned and the combo falls back to its default route.
	Timeout string `yaml:"timeout,omitempty"`
	// MinConfidence is the floor below which Jev's answer is not trusted and
	// the default route is used instead. Default: 0.5.
	MinConfidence string `yaml:"min_confidence,omitempty"`
	// Question is the instruction Jev answers. Default: a generic
	// "which model should handle this request?" prompt.
	Question string `yaml:"question,omitempty"`
}

// ParsedTimeout returns the decision budget, falling back to 4s.
func (j SmartJev) ParsedTimeout() time.Duration {
	if j.Timeout == "" {
		return 4 * time.Second
	}
	d, err := time.ParseDuration(j.Timeout)
	if err != nil || d <= 0 {
		return 4 * time.Second
	}
	return d
}

// ParsedMinConfidence returns the confidence floor, falling back to 0.5.
func (j SmartJev) ParsedMinConfidence() float64 {
	if j.MinConfidence == "" {
		return 0.5
	}
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(j.MinConfidence), "%g", &f); err != nil {
		return 0.5
	}
	if f < 0 || f > 1 {
		return 0.5
	}
	return f
}

// ModelOrDefault returns the Jev model to call.
func (j SmartJev) ModelOrDefault() string {
	if strings.TrimSpace(j.Model) == "" {
		return "jev-latest"
	}
	return strings.TrimSpace(j.Model)
}

// QuestionOrDefault returns the instruction Jev answers.
func (j SmartJev) QuestionOrDefault() string {
	if strings.TrimSpace(j.Question) == "" {
		return "Which model should handle this request?"
	}
	return strings.TrimSpace(j.Question)
}

// SmartRoute is one option in a smart combo. Jev chooses between routes by
// reading Criteria; Destination is where OmniGo sends the request once the
// route wins. It is not sent to Jev.
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

// IsSmart reports whether this combo routes through Jev.
func (c Combo) IsSmart() bool { return c.Strategy == StrategySmart }

// StrategySmart is the Jev-backed routing strategy.
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
	if c.Jev.Timeout != "" {
		d, err := time.ParseDuration(c.Jev.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("combo %q: invalid jev timeout %q", c.Name, c.Jev.Timeout)
		}
	}
	if c.Jev.MinConfidence != "" {
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(c.Jev.MinConfidence), "%g", &f); err != nil || f < 0 || f > 1 {
			return fmt.Errorf("combo %q: invalid jev min_confidence %q (want 0..1)", c.Name, c.Jev.MinConfidence)
		}
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
