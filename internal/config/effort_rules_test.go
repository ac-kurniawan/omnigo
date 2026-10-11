package config

import "testing"

// The relaxation must not accept an effort level the model's own profile does
// not support: gpt-5.5 is low/medium/high/xhigh, so -ultra is a typo.
func TestValidateRejectsAnEffortLevelTheFamilyDoesNotSupport(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{{Name: "cx", Type: "codex", Models: []string{"gpt-5.5"}}},
		Combos: []Combo{{Name: "a", Strategy: "priority", Targets: []ComboTarget{
			{Provider: "cx", Model: "gpt-5.5-ultra"},
		}}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected gpt-5.5-ultra to be rejected: gpt-5.5 supports low/medium/high/xhigh only")
	}
}

// A capability marker is a real upstream id, not a synthesised clone, so it must
// be listed exactly like any other catalog id.
func TestValidateRequiresCapabilityMarkersToBeListed(t *testing.T) {
	absent := &Config{
		Providers: []Provider{{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.1-pro-low"}}},
		Combos: []Combo{{Name: "a", Strategy: "priority", Targets: []ComboTarget{
			{Provider: "agy", Model: "gemini-3.1-pro-thinking"},
		}}},
	}
	if err := absent.Validate(); err == nil {
		t.Fatal("expected an unlisted -thinking marker to be rejected")
	}

	listed := &Config{
		Providers: []Provider{{Name: "agy", Type: "antigravity", Models: []string{"claude-opus-4-6-thinking"}}},
		Combos: []Combo{{Name: "a", Strategy: "priority", Targets: []ComboTarget{
			{Provider: "agy", Model: "claude-opus-4-6-thinking"},
		}}},
	}
	if err := listed.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// Validate and PruneComboTargets must agree: a target one accepts can never be
// silently dropped by the other on the next catalog refresh.
func TestValidateAndPruneAgreeOnEveryTarget(t *testing.T) {
	cases := []struct {
		providerType string
		catalog      []string
		target       string
	}{
		{"codex", []string{"gpt-5.5"}, "gpt-5.5"},
		{"codex", []string{"gpt-5.5"}, "gpt-5.5-high"},
		{"codex", []string{"gpt-5.5"}, "gpt-5.5-ultra"},
		{"codex", []string{"gpt-5.5-high"}, "gpt-5.5"},
		{"codex", []string{"gpt-5.5-high"}, "gpt-5.5-medium"},
		{"antigravity", []string{"gemini-3.7-flash-low", "gemini-3.7-flash-high"}, "gemini-3.7-flash"},
		{"antigravity", []string{"gemini-3.7-flash-low", "gemini-3.7-flash-high"}, "gemini-3.7-flash-medium"},
		{"antigravity", []string{"claude-opus-4-6-thinking"}, "claude-opus-4-6"},
		{"antigravity", []string{"claude-opus-4-6-thinking"}, "claude-opus-4-6-thinking"},
		{"antigravity", []string{"gemini-3.1-pro-low"}, "gemini-3.1-pro-thinking"},
		{"openai", []string{"gpt-4o"}, "gpt-4o-high"},
		{"codex", []string{"gpt-5.5"}, "gpt-6-luna-high"},
	}
	for _, tc := range cases {
		build := func() *Config {
			return &Config{
				Providers: []Provider{{Name: "p", Type: tc.providerType, Models: append([]string(nil), tc.catalog...)}},
				Combos:    []Combo{{Name: "a", Strategy: "priority", Targets: []ComboTarget{{Provider: "p", Model: tc.target}}}},
			}
		}
		validateOK := build().Validate() == nil

		pc := build()
		PruneComboTargets(pc)
		pruneKept := len(pc.Combos) == 1 && len(pc.Combos[0].Targets) == 1

		if validateOK != pruneKept {
			t.Errorf("type=%s catalog=%v target=%q: validate=%v pruneKept=%v (must agree)",
				tc.providerType, tc.catalog, tc.target, validateOK, pruneKept)
		}
	}
}
