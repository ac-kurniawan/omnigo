package config

import (
	"strings"
	"testing"
)

// An effort stored in the model id was the old encoding. It is rejected with a
// message that says where the level belongs, so an operator can fix the combo.
func TestValidateRejectsARemovedEffortSuffix(t *testing.T) {
	for _, tc := range []struct {
		providerType string
		catalog      []string
		model        string
		wantBase     string
		wantLevel    string
	}{
		{"codex", []string{"gpt-5.5"}, "gpt-5.5-high", "gpt-5.5", "high"},
		{"openai", []string{"o3-mini"}, "o3-mini-ultra", "o3-mini", "ultra"},
		{"antigravity", []string{"gemini-3.7-flash-low"}, "gemini-3.7-flash-high", "gemini-3.7-flash", "high"},
	} {
		cfg := &Config{
			Providers: []Provider{{Name: "p", Type: tc.providerType, Models: tc.catalog}},
			Combos:    []Combo{{Name: "a", Strategy: "priority", Targets: []ComboTarget{{Provider: "p", Model: tc.model}}}},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("%s: expected %q to be rejected", tc.providerType, tc.model)
		}
		msg := err.Error()
		if !strings.Contains(msg, "removed effort suffix") || !strings.Contains(msg, tc.wantBase) || !strings.Contains(msg, tc.wantLevel) {
			t.Fatalf("%s: error %q does not name the suffix, base, and level", tc.providerType, msg)
		}
	}
}

// A literally listed id that ends in a level token is a real upstream model, not
// an old clone: it must keep validating.
func TestValidateAcceptsAListedSuffixedModel(t *testing.T) {
	for _, tc := range []struct {
		providerType string
		model        string
	}{
		{"codex", "gpt-oss-120b-medium"},
		{"antigravity", "gemini-3.7-flash-high"},
	} {
		cfg := &Config{
			Providers: []Provider{{Name: "p", Type: tc.providerType, Models: []string{tc.model}}},
			Combos:    []Combo{{Name: "a", Strategy: "priority", Targets: []ComboTarget{{Provider: "p", Model: tc.model}}}},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s: listed id %q rejected: %v", tc.providerType, tc.model, err)
		}
	}
}

// Validate and PruneComboTargets must agree: a target one accepts can never be
// silently dropped by the other on the next catalog refresh.
func TestValidateAndPruneAgreeOnEveryTarget(t *testing.T) {
	cases := []struct {
		providerType string
		catalog      []string
		model        string
		effort       string
	}{
		{"codex", []string{"gpt-5.5"}, "gpt-5.5", ""},
		{"codex", []string{"gpt-5.5"}, "gpt-5.5", "high"},
		{"codex", []string{"gpt-5.5"}, "gpt-5.5", "ultra"},
		{"codex", []string{"gpt-5.5"}, "gpt-5.5-high", ""},
		{"codex", []string{"gpt-oss-120b-medium"}, "gpt-oss-120b-medium", ""},
		{"codex", []string{"gpt-oss-120b-medium"}, "gpt-oss-120b-medium", "high"},
		{"antigravity", []string{"gemini-3.7-flash-low", "gemini-3.7-flash-high"}, "gemini-3.7-flash", "medium"},
		{"antigravity", []string{"gemini-3.7-flash-low", "gemini-3.7-flash-high"}, "gemini-3.7-flash", ""},
		{"antigravity", []string{"gemini-3.7-flash-low", "gemini-3.7-flash-high"}, "gemini-3.7-flash", "max"},
		{"antigravity", []string{"claude-opus-4-6-thinking"}, "claude-opus-4-6-thinking", ""},
		{"antigravity", []string{"claude-opus-4-6-thinking"}, "claude-opus-4-6", ""},
		{"openai", []string{"gpt-4o"}, "gpt-4o", ""},
		{"openai", []string{"gpt-4o"}, "gpt-4o", "high"},
		{"openai", []string{"gpt-4o"}, "gpt-4o-high", ""},
		{"codex", []string{"gpt-5.5"}, "gpt-6-luna", ""},
	}
	for _, tc := range cases {
		build := func() *Config {
			return &Config{
				Providers: []Provider{{Name: "p", Type: tc.providerType, Models: append([]string(nil), tc.catalog...)}},
				Combos:    []Combo{{Name: "a", Strategy: "priority", Targets: []ComboTarget{{Provider: "p", Model: tc.model, Effort: tc.effort}}}},
			}
		}
		validateOK := build().Validate() == nil

		pc := build()
		PruneComboTargets(pc)
		pruneKept := len(pc.Combos) == 1 && len(pc.Combos[0].Targets) == 1

		if validateOK != pruneKept {
			t.Errorf("type=%s catalog=%v target=%q effort=%q: validate=%v pruneKept=%v (must agree)",
				tc.providerType, tc.catalog, tc.model, tc.effort, validateOK, pruneKept)
		}
	}
}

// A capability marker is a real upstream id, not an effort variant, so it must
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
