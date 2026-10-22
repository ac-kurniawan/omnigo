package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A smart combo has no targets of its own, so the prune that removes combos
// whose targets vanished must leave it alone.
func TestPruneKeepsSmartCombos(t *testing.T) {
	c := &Config{
		Providers: []Provider{{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}}},
		Combos: []Combo{
			{Name: "combo/smart", Strategy: StrategySmart, DefaultRoute: "flash", Routes: []SmartRoute{
				{Name: "flash", Criteria: "simple", Destination: "combo/flash"},
				{Name: "pro", Criteria: "hard", Destination: "combo/pro"},
			}},
			{Name: "combo/flash", Strategy: "priority", Targets: []ComboTarget{{Provider: "openai", Model: "gpt-4o"}}},
		},
	}
	PruneComboTargets(c)
	if _, ok := FindCombo(c, "combo/smart"); !ok {
		t.Fatal("smart combo was pruned")
	}
}

// A smart combo whose destination provider disappears keeps its routes: the
// destination is resolved at request time, not at prune time.
func TestPruneKeepsSmartRoutesWhenProviderVanishes(t *testing.T) {
	c := &Config{
		Providers: []Provider{{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}}},
		Combos: []Combo{
			{Name: "combo/smart", Strategy: StrategySmart, DefaultRoute: "flash", Routes: []SmartRoute{
				{Name: "flash", Criteria: "simple", Destination: "openai/gpt-4o"},
			}},
		},
	}
	PruneComboTargets(c)
	combo, ok := FindCombo(c, "combo/smart")
	if !ok {
		t.Fatal("smart combo was pruned")
	}
	if len(combo.Routes) != 1 {
		t.Fatalf("routes = %d, want 1", len(combo.Routes))
	}
}

// A plain combo whose targets all vanished is removed, as before.
func TestPruneStillRemovesEmptyPlainCombo(t *testing.T) {
	c := &Config{
		Providers: []Provider{{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}}},
		Combos: []Combo{
			{Name: "combo/plain", Strategy: "priority", Targets: []ComboTarget{{Provider: "gone", Model: "x"}}},
		},
	}
	PruneComboTargets(c)
	if _, ok := FindCombo(c, "combo/plain"); ok {
		t.Fatal("plain combo with no valid targets should be removed")
	}
}

// SetSmartCombo stores the routes, default route, and Jev settings of a smart
// combo, and leaves the rest of the config untouched.
func TestSetSmartCombo(t *testing.T) {
	c := &Config{
		Providers: []Provider{{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}}},
		Combos: []Combo{
			{Name: "combo/smart", Strategy: StrategySmart},
			{Name: "combo/flash", Strategy: "priority", Targets: []ComboTarget{{Provider: "openai", Model: "gpt-4o"}}},
		},
	}
	routes := []SmartRoute{
		{Name: "flash", Criteria: "simple", Destination: "combo/flash"},
		{Name: "pro", Criteria: "hard", Destination: "openai/gpt-4o"},
	}
	err := SetSmartCombo(c, "combo/smart", routes, "flash", SmartJev{Model: "jev-latest", MinConfidence: "0.6", Timeout: "5s"})
	if err != nil {
		t.Fatalf("SetSmartCombo: %v", err)
	}
	combo, ok := FindCombo(c, "combo/smart")
	if !ok {
		t.Fatal("combo not found")
	}
	if len(combo.Routes) != 2 {
		t.Fatalf("routes = %d, want 2", len(combo.Routes))
	}
	if combo.DefaultRoute != "flash" {
		t.Errorf("default route = %q", combo.DefaultRoute)
	}
	if combo.Jev.MinConfidence != "0.6" {
		t.Errorf("min confidence = %q", combo.Jev.MinConfidence)
	}
	if combo.Jev.ParsedMinConfidence() != 0.6 {
		t.Errorf("parsed min confidence = %v", combo.Jev.ParsedMinConfidence())
	}
}

// SetSmartCombo rejects a default route that is not one of the routes.
func TestSetSmartComboRejectsUnknownDefaultRoute(t *testing.T) {
	c := &Config{
		Providers: []Provider{{Name: "openai", Type: "openai", Models: []string{"gpt-4o"}}},
		Combos: []Combo{
			{Name: "combo/smart", Strategy: StrategySmart},
			{Name: "combo/flash", Strategy: "priority", Targets: []ComboTarget{{Provider: "openai", Model: "gpt-4o"}}},
		},
	}
	err := SetSmartCombo(c, "combo/smart", []SmartRoute{{Name: "flash", Criteria: "s", Destination: "combo/flash"}}, "nope", SmartJev{})
	if err == nil {
		t.Fatal("expected an error for an unknown default route")
	}
}

// UpdateSmartCombo writes the smart combo to disk and it survives a reload.
func TestUpdateSmartComboPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	base := "server:\n  port: 8080\nproviders:\n  - name: openai\n    type: openai\n    base_url: https://x\n    models:\n      - gpt-4o\ncombos:\n  - name: combo/smart\n    strategy: smart\n    default_route: flash\n    routes:\n      - name: flash\n        criteria: simple\n        destination: openai/gpt-4o\n"
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	routes := []SmartRoute{{Name: "flash", Criteria: "simple", Destination: "openai/gpt-4o"}}
	if err := UpdateSmartCombo(path, "combo/smart", routes, "flash", SmartJev{Model: "jev-latest"}); err != nil {
		t.Fatalf("UpdateSmartCombo: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	combo, ok := FindCombo(loaded, "combo/smart")
	if !ok {
		t.Fatal("combo not found after reload")
	}
	if len(combo.Routes) != 1 || combo.Routes[0].Destination != "openai/gpt-4o" {
		t.Fatalf("routes = %+v", combo.Routes)
	}
	if combo.Jev.Model != "jev-latest" {
		t.Errorf("jev model = %q", combo.Jev.Model)
	}
}
