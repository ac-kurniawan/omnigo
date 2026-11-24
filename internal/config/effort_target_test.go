package config

import "testing"

// A target stores the base model plus an effort, so Antigravity's suffixed
// upstream family (gemini-3.7-flash-low/high in the catalog) is reachable
// through its plain base, and a body-effort provider through its own base.
func TestValidateAcceptsABaseModelWithAnEffort(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{
			{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.7-flash-low", "gemini-3.7-flash-high"}},
			{Name: "cx", Type: "codex", Models: []string{"gpt-5.5"}},
		},
		Combos: []Combo{
			{Name: "a", Strategy: "priority", Targets: []ComboTarget{
				{Provider: "agy", Model: "gemini-3.7-flash", Effort: "medium"},
				{Provider: "cx", Model: "gpt-5.5", Effort: "xhigh"},
				{Provider: "cx", Model: "gpt-5.5"},
			}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// A capability marker (-thinking, -tiered) is a real upstream id, not an effort
// variant: it validates when the catalog actually lists it.
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

// A model unrelated to the catalog is still a typo and must be rejected.
func TestValidateStillRejectsAnUnrelatedModel(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{{Name: "cx", Type: "codex", Models: []string{"gpt-5.5"}}},
		Combos: []Combo{
			{Name: "a", Strategy: "priority", Targets: []ComboTarget{{Provider: "cx", Model: "gpt-6-luna"}}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for a target whose model is absent")
	}
}

// A model id that merely looks like a suffix must not be accepted: the id is
// the model and it is not listed.
func TestValidateRejectsAModelWhoseBaseIsAbsent(t *testing.T) {
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

// An empty catalog means "not fetched yet" and must keep accepting anything.
func TestValidateAcceptsAnythingAgainstAnEmptyCatalog(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{{Name: "p", Type: "codex"}},
		Combos: []Combo{
			{Name: "a", Strategy: "priority", Targets: []ComboTarget{{Provider: "p", Model: "gpt-5.5"}}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// An effort on a model with no reasoning profile is rejected: the target would
// silently lose the level at dispatch.
func TestValidateRejectsAnEffortOnANonReasoningModel(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{{Name: "agy", Type: "antigravity", Models: []string{"claude-sonnet-4-6"}}},
		Combos: []Combo{
			{Name: "a", Strategy: "priority", Targets: []ComboTarget{{Provider: "agy", Model: "claude-sonnet-4-6", Effort: "high"}}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an effort on a non-reasoning model to be rejected")
	}
}

func TestPruneComboTargetsKeepsAnEffortTarget(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.7-flash-high"}}},
		Combos: []Combo{
			{Name: "a", Strategy: "priority", Targets: []ComboTarget{
				{Provider: "agy", Model: "gemini-3.7-flash", Effort: "medium"},
			}},
		},
	}
	PruneComboTargets(cfg)
	got := cfg.Combos
	if len(got) != 1 || len(got[0].Targets) != 1 || got[0].Targets[0].Model != "gemini-3.7-flash" || got[0].Targets[0].Effort != "medium" {
		t.Fatalf("prune dropped an effort target: %+v", got)
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
