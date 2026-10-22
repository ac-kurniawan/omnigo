package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SetSmartCombo replaces the routes, default route, and Jev settings of a
// smart combo in memory. The combo must already exist and use the smart
// strategy; a plain combo is not converted here, because that would silently
// discard its target chain.
//
// The default route must name one of the routes, so a fallback always resolves
// to a real destination.
func SetSmartCombo(c *Config, name string, routes []SmartRoute, defaultRoute string, jev SmartJev) error {
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
	if err := validateSmartJevFields(name, jev); err != nil {
		return err
	}
	for i := range c.Combos {
		if c.Combos[i].Name == name {
			c.Combos[i].Routes = routes
			c.Combos[i].DefaultRoute = defaultRoute
			c.Combos[i].Jev = jev
			return nil
		}
	}
	return fmt.Errorf("combo %q not found", name)
}

// UpdateSmartCombo writes a smart combo's routes, default route, and Jev
// settings to disk.
func UpdateSmartCombo(path, name string, routes []SmartRoute, defaultRoute string, jev SmartJev) error {
	return Mutate(path, func(c *Config) error {
		return SetSmartCombo(c, name, routes, defaultRoute, jev)
	})
}

// validateSmartJevFields checks the Jev settings that have their own syntax.
func validateSmartJevFields(comboName string, jev SmartJev) error {
	if jev.Timeout != "" {
		if _, err := time.ParseDuration(jev.Timeout); err != nil {
			return fmt.Errorf("combo %q: invalid jev timeout %q", comboName, jev.Timeout)
		}
	}
	if jev.MinConfidence != "" {
		if _, err := parseConfidence(jev.MinConfidence); err != nil {
			return fmt.Errorf("combo %q: invalid jev min_confidence %q (want 0..1)", comboName, jev.MinConfidence)
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
