package effort

import (
	"strings"
	"testing"
)

func TestSplitStripsConcreteEffortLevels(t *testing.T) {
	cases := []struct{ in, base, suffix string }{
		{"gpt-5.5-high", "gpt-5.5", "high"},
		{"gpt-5.5-low", "gpt-5.5", "low"},
		{"gpt-5.5-medium", "gpt-5.5", "medium"},
		{"gpt-6-astra-xhigh", "gpt-6-astra", "xhigh"},
		{"gpt-6-astra-ultra", "gpt-6-astra", "ultra"},
		{"gpt-6-astra-max", "gpt-6-astra", "max"},
		{"gemini-3.7-flash-extra-low", "gemini-3.7-flash", "extra-low"},
		{"gpt-oss-120b-medium", "gpt-oss-120b", "medium"},
	}
	for _, tc := range cases {
		base, suffix, found := Split(tc.in)
		if !found || base != tc.base || suffix != tc.suffix {
			t.Fatalf("Split(%q) = (%q, %q, %v), want (%q, %q, true)", tc.in, base, suffix, found, tc.base, tc.suffix)
		}
	}
}

func TestSplitPrefersTheLongestSuffix(t *testing.T) {
	// "extra-low" ends in "low"; stripping "low" would leave "gemini-3.7-flash-extra".
	base, suffix, found := Split("gemini-3.7-flash-extra-low")
	if !found || base != "gemini-3.7-flash" || suffix != "extra-low" {
		t.Fatalf("Split = (%q, %q, %v), want (gemini-3.7-flash, extra-low, true)", base, suffix, found)
	}
}

func TestSplitStripsCapabilityMarkersWithoutALevel(t *testing.T) {
	for _, in := range []string{"claude-opus-4-6-thinking", "gemini-3.8-flash-tiered"} {
		_, suffix, found := Split(in)
		if !found {
			t.Fatalf("Split(%q) found = false, want true", in)
		}
		if Level(suffix) {
			t.Fatalf("Split(%q) suffix %q reported as an effort level, want marker", in, suffix)
		}
	}
	base, _, found := Split("claude-opus-4-6-thinking")
	if !found || base != "claude-opus-4-6" {
		t.Fatalf("Split base = %q, found = %v, want claude-opus-4-6, true", base, found)
	}
	base, _, found = Split("gemini-3.8-flash-tiered")
	if !found || base != "gemini-3.8-flash" {
		t.Fatalf("Split base = %q, found = %v, want gemini-3.8-flash, true", base, found)
	}
}

func TestSplitLeavesAPlainIDAlone(t *testing.T) {
	for _, in := range []string{"gpt-5.5", "gemini-3.7-flash", "claude-sonnet-4-6", "gpt-oss-120b"} {
		base, suffix, found := Split(in)
		if found || base != in || suffix != "" {
			t.Fatalf("Split(%q) = (%q, %q, %v), want (%q, \"\", false)", in, base, suffix, found, in)
		}
	}
}

func TestSplitIgnoresNonTrailingAndUnknownSuffixes(t *testing.T) {
	// A suffix only counts at the end of the id, and "lite"/"preview" are not
	// effort levels or capability markers.
	for _, in := range []string{"model-high-preview", "gemini-3.1-flash-lite", "gpt-4o", "claude-opus-4-6"} {
		base, suffix, found := Split(in)
		if found || base != in || suffix != "" {
			t.Fatalf("Split(%q) = (%q, %q, %v), want (%q, \"\", false)", in, base, suffix, found, in)
		}
	}
}

func TestSplitRefusesToEmptyTheBase(t *testing.T) {
	for _, in := range []string{"low", "high", "thinking"} {
		base, suffix, found := Split(in)
		if found || base != in || suffix != "" {
			t.Fatalf("Split(%q) = (%q, %q, %v), want (%q, \"\", false)", in, base, suffix, found, in)
		}
	}
}

func TestLevelRecognisesOnlyConcreteEfforts(t *testing.T) {
	for _, level := range []string{"low", "medium", "high", "extra-low", "xhigh", "max", "ultra"} {
		if !Level(level) {
			t.Fatalf("Level(%q) = false, want true", level)
		}
	}
	for _, marker := range []string{"thinking", "tiered", "lite", "", "preview"} {
		if Level(marker) {
			t.Fatalf("Level(%q) = true, want false", marker)
		}
	}
}

func TestBaseAndApplyRoundTrip(t *testing.T) {
	if got := Base("gemini-3.7-flash-medium"); got != "gemini-3.7-flash" {
		t.Fatalf("Base = %q, want gemini-3.7-flash", got)
	}
	if got := Base("gpt-5.5"); got != "gpt-5.5" {
		t.Fatalf("Base = %q, want gpt-5.5", got)
	}
	if got := Apply("gemini-3.7-flash", "high"); got != "gemini-3.7-flash-high" {
		t.Fatalf("Apply = %q, want gemini-3.7-flash-high", got)
	}
	if got := Apply("gpt-5.5", "xhigh"); got != "gpt-5.5-xhigh" {
		t.Fatalf("Apply = %q, want gpt-5.5-xhigh", got)
	}
}

// An id that is itself an effort level has no base to strip to: "extra-low"
// must not split into base "extra".
func TestSplitTreatsABareLevelAsAnId(t *testing.T) {
	for _, in := range []string{"low", "medium", "high", "extra-low", "xhigh", "max", "ultra"} {
		base, suffix, found := Split(in)
		if found || base != in || suffix != "" {
			t.Fatalf("Split(%q) = (%q, %q, %v), want (%q, \"\", false)", in, base, suffix, found, in)
		}
		if got := Base(in); got != in {
			t.Fatalf("Base(%q) = %q, want %q", in, got, in)
		}
	}
}

// The "-extra-low" / "-low" overlap relies on the levels being ordered longest
// first; guard the invariant the doc comment promises.
func TestLevelsAreOrderedLongestFirst(t *testing.T) {
	for i := 0; i < len(levels); i++ {
		for j := 0; j < len(levels); j++ {
			if i == j || !strings.HasSuffix(levels[i], levels[j]) || len(levels[i]) == len(levels[j]) {
				continue
			}
			if i > j {
				t.Fatalf("levels[%d]=%q must precede levels[%d]=%q (longest first)", i, levels[i], j, levels[j])
			}
		}
	}
}
