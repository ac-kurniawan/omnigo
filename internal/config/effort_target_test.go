package config

import "testing"

// A combo target may name an effort variant of a listed model: the picker
// synthesises `gemini-3.7-flash-high` from a catalog that lists the level
// variants, and `gpt-5.5-low` from a base whose effort rides in the body.
func TestValidateAcceptsEffortVariantOfAListedModel(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{
			{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.7-flash-low", "gemini-3.7-flash-high"}},
			{Name: "cx", Type: "codex", Models: []string{"gpt-5.5"}},
		},
		Combos: []Combo{
			{Name: "a", Strategy: "priority", Targets: []ComboTarget{
				{Provider: "agy", Model: "gemini-3.7-flash-medium"},
				{Provider: "cx", Model: "gpt-5.5-xhigh"},
			}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// A capability marker (-thinking, -tiered) is a real upstream id, not a
// synthesised clone: it validates when the catalog actually lists it.
func TestValidateAcceptsCapabilityVariantOfAListedModel(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{
			{Name: "agy", Type: "antigravity", Models: []string{"claude-opus-4-6-thinking", "gemini-3.8-flash-tiered"}},
		},
		Combos: []Combo{
			{Name: "a", Strategy: "priority", Targets: []ComboTarget{
				{Provider: "agy", Model: "claude-opus-4-6-thinking"},
				{Provider: "agy", Model: "gemini-3.8-flash-tiered"},
			}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// The relaxation only reaches a variant whose base is present. An id unrelated
// to the catalog is still a typo and must be rejected.
func TestValidateStillRejectsAnUnrelatedModel(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{{Name: "cx", Type: "codex", Models: []string{"gpt-5.5"}}},
		Combos: []Combo{
			{Name: "a", Strategy: "priority", Targets: []ComboTarget{{Provider: "cx", Model: "gpt-6-luna-high"}}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for a target whose base model is absent")
	}
}

// A plain id that merely looks like a suffix ("gpt-4o" ends in nothing, but
// "model-high" would strip to "model") must not be accepted unless the base is
// actually listed.
func TestValidateRejectsAVariantWhoseBaseIsAbsent(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{{Name: "p", Type: "openai", Models: []string{"gpt-4o"}}},
		Combos: []Combo{
			{Name: "a", Strategy: "priority", Targets: []ComboTarget{{Provider: "p", Model: "gemini-3.7-flash-high"}}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error: gemini-3.7-flash is not in the catalog")
	}
}

// An empty catalog means "not fetched yet" and must keep accepting anything, as
// it did before this change.
func TestValidateAcceptsAnythingAgainstAnEmptyCatalog(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{{Name: "p", Type: "codex"}},
		Combos: []Combo{
			{Name: "a", Strategy: "priority", Targets: []ComboTarget{{Provider: "p", Model: "gpt-5.5-high"}}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestPruneComboTargetsKeepsAnEffortVariant(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.7-flash-high"}}},
		Combos: []Combo{
			{Name: "a", Strategy: "priority", Targets: []ComboTarget{
				{Provider: "agy", Model: "gemini-3.7-flash-medium"},
			}},
		},
	}
	PruneComboTargets(cfg)
	if len(cfg.Combos) != 1 || len(cfg.Combos[0].Targets) != 1 || cfg.Combos[0].Targets[0].Model != "gemini-3.7-flash-medium" {
		t.Fatalf("prune dropped an effort variant: %+v", cfg.Combos)
	}
}

func TestPruneComboTargetsStillDropsAnUnrelatedModel(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.7-flash-high"}}},
		Combos: []Combo{
			{Name: "a", Strategy: "priority", Targets: []ComboTarget{
				{Provider: "agy", Model: "claude-sonnet-4-6"},
			}},
		},
	}
	PruneComboTargets(cfg)
	if len(cfg.Combos) != 0 {
		t.Fatalf("prune kept an unrelated model: %+v", cfg.Combos)
	}
}
