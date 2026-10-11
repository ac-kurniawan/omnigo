package provider

import (
	"reflect"
	"testing"
)

func TestEffortVariantsUseTheModelsRealLevels(t *testing.T) {
	cases := []struct {
		name        string
		providerTyp string
		base        string
		catalog     []string
		want        []string
	}{
		{
			name:        "codex profile levels",
			providerTyp: "codex",
			base:        "gpt-5.5",
			want:        []string{"low", "medium", "high", "xhigh"},
		},
		{
			name:        "codex richest profile",
			providerTyp: "codex",
			base:        "gpt-6-astra",
			want:        []string{"low", "medium", "high", "xhigh", "max", "ultra"},
		},
		{
			name:        "generic openai reasoning id",
			providerTyp: "openai",
			base:        "o3-mini",
			want:        []string{"low", "medium", "high"},
		},
		{
			name:        "antigravity base gated on an existing level variant",
			providerTyp: "antigravity",
			base:        "gemini-3.7-flash",
			catalog:     []string{"gemini-3.7-flash-low", "gemini-3.7-flash-medium", "gemini-3.7-flash-high"},
			want:        []string{"low", "medium", "high"},
		},
		{
			name:        "antigravity base with a single level variant still offers the full set",
			providerTyp: "antigravity",
			base:        "gemini-3.1-pro",
			catalog:     []string{"gemini-3.1-pro-low"},
			want:        []string{"low", "medium", "high"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EffortVariants(tc.providerTyp, tc.base, tc.catalog)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("EffortVariants(%q, %q, %v) = %v, want %v", tc.providerTyp, tc.base, tc.catalog, got, tc.want)
			}
		})
	}
}

func TestEffortVariantsDeclineUncloneableBases(t *testing.T) {
	cases := []struct {
		name        string
		providerTyp string
		base        string
		catalog     []string
	}{
		{
			name:        "plain openai model",
			providerTyp: "openai",
			base:        "gpt-4o",
		},
		{
			name:        "antigravity base with no level variant in the catalog",
			providerTyp: "antigravity",
			base:        "claude-sonnet-4-6",
			catalog:     []string{"claude-sonnet-4-6", "claude-opus-4-6-thinking"},
		},
		{
			name:        "antigravity thinking marker is not an effort level",
			providerTyp: "antigravity",
			base:        "claude-opus-4-6",
			catalog:     []string{"claude-opus-4-6-thinking"},
		},
		{
			name:        "unknown provider type",
			providerTyp: "mystery",
			base:        "gpt-5.5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffortVariants(tc.providerTyp, tc.base, tc.catalog); got != nil {
				t.Fatalf("EffortVariants(%q, %q, %v) = %v, want nil", tc.providerTyp, tc.base, tc.catalog, got)
			}
		})
	}
}

func TestEffortVariantsSkipTheBaseItself(t *testing.T) {
	// The picker renders the plain catalog entry separately, so a variant that
	// equals the base would duplicate it.
	got := EffortVariants("antigravity", "gemini-3.7-flash", []string{"gemini-3.7-flash", "gemini-3.7-flash-high"})
	want := []string{"low", "medium", "high"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EffortVariants = %v, want %v", got, want)
	}
}

func TestEffortVariantsNormaliseASuffixedInput(t *testing.T) {
	got := EffortVariants("codex", "gpt-5.5-high", nil)
	want := []string{"low", "medium", "high", "xhigh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EffortVariants = %v, want %v", got, want)
	}
}
