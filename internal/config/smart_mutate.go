package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SetSmartCombo replaces the routes, default route, and decision settings of a
// smart combo in memory. The combo must already exist and use the smart
// strategy; a plain combo is not converted here, because that would silently
// discard its target chain.
//
// The default route must name one of the routes, so a fallback always resolves
// to a real destination.
func SetSmartCombo(c *Config, name string, routes []SmartRoute, defaultRoute string, decision SmartDecision) error {
	if len(routes) == 0 {
		return fmt.Errorf("combo %q: at least one route is required", name)
	}
	found := false
	for i := range c.Combos {
		if c.Combos[i].Name != name {
			continue
		}
		found = true
		if !c.Combos[i].IsSmart() {
			return fmt.Errorf("combo %q: not a smart combo", name)
		}
		break
	}
	if !found {
		return fmt.Errorf("combo %q not found", name)
	}
	if defaultRoute == "" {
		return fmt.Errorf("combo %q: default_route is required", name)
	}
	known := false
	for _, route := range routes {
		if route.Name == defaultRoute {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("combo %q: default_route %q is not one of the routes", name, defaultRoute)
	}
	if err := validateSmartDecisionFields(name, decision); err != nil {
		return err
	}
	if err := validateSmartWeights(name, routes, decision); err != nil {
		return err
	}
	for i := range c.Combos {
		if c.Combos[i].Name == name {
			c.Combos[i].Routes = routes
			c.Combos[i].DefaultRoute = defaultRoute
			c.Combos[i].Decision = decision
			return nil
		}
	}
	return fmt.Errorf("combo %q not found", name)
}

// UpdateSmartCombo writes a smart combo's routes, default route, and decision
// settings to disk.
func UpdateSmartCombo(path, name string, routes []SmartRoute, defaultRoute string, decision SmartDecision) error {
	return Mutate(path, func(c *Config) error {
		return SetSmartCombo(c, name, routes, defaultRoute, decision)
	})
}

// validateSmartDecisionFields checks the decision settings that have their own
// syntax. It is shared by the whole-config path and the dashboard path so the
// two cannot drift.
func validateSmartDecisionFields(comboName string, decision SmartDecision) error {
	if decision.Timeout != "" {
		if _, err := time.ParseDuration(decision.Timeout); err != nil {
			return fmt.Errorf("combo %q: invalid decision timeout %q", comboName, decision.Timeout)
		}
	}
	if decision.MinConfidence != "" {
		if _, err := parseConfidence(decision.MinConfidence); err != nil {
			return fmt.Errorf("combo %q: invalid decision min_confidence %q (want 0..1)", comboName, decision.MinConfidence)
		}
	}
	switch strings.TrimSpace(decision.Mode) {
	case "", DecisionModeChoice, DecisionModeFanout:
	default:
		return fmt.Errorf("combo %q: invalid decision mode %q (want %q or %q)", comboName, decision.Mode, DecisionModeChoice, DecisionModeFanout)
	}
	return nil
}

// validateSmartWeights rejects a weight for a route that does not exist, which
// would otherwise be silently ignored and quietly do nothing.
func validateSmartWeights(comboName string, routes []SmartRoute, decision SmartDecision) error {
	if len(decision.Weights) == 0 {
		return nil
	}
	known := make(map[string]bool, len(routes))
	for _, route := range routes {
		known[route.Name] = true
	}
	for name, weight := range decision.Weights {
		if !known[name] {
			return fmt.Errorf("combo %q: weight set for unknown route %q", comboName, name)
		}
		if weight <= 0 {
			return fmt.Errorf("combo %q: weight for route %q must be positive", comboName, name)
		}
	}
	return nil
}

// parseConfidence parses a 0..1 confidence floor.
func parseConfidence(value string) (float64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("empty confidence")
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, err
	}
	if parsed < 0 || parsed > 1 {
		return 0, fmt.Errorf("confidence %q out of range", value)
	}
	return parsed, nil
}

// ParseSmartRoutes reads the route set from a request form. Each route is
// given as name|criteria|destination, so a single form value carries all
// three fields and the order of the routes is preserved.
func ParseSmartRoutes(raw string) []SmartRoute {
	var routes []SmartRoute
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		routes = append(routes, SmartRoute{
			Name:        strings.TrimSpace(parts[0]),
			Criteria:    strings.TrimSpace(parts[1]),
			Destination: strings.TrimSpace(parts[2]),
		})
	}
	return routes
}

// ParseSmartWeights reads per-route weights from a request form. Each entry
// is "route=weight", separated by newlines or commas. An entry that does not
// parse is skipped rather than failing the whole form, and the weights are
// validated against the routes afterwards.
func ParseSmartWeights(raw string) map[string]float64 {
	weights := map[string]float64{}
	for _, line := range strings.FieldsFunc(raw, func(c rune) bool {
		return c == '\n' || c == '\r' || c == ','
	}) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		weight, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			continue
		}
		weights[name] = weight
	}
	if len(weights) == 0 {
		return nil
	}
	return weights
}
