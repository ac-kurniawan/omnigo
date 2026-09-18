package quota

import (
	"testing"
	"time"
)

func TestStatusConstants(t *testing.T) {
	if StatusAvailable != "available" || StatusExhausted != "exhausted" || StatusUnavailable != "unavailable" {
		t.Fatalf("status values drifted: %q %q %q", StatusAvailable, StatusExhausted, StatusUnavailable)
	}
}

// Codex reports the windows as "primary"/"secondary"; the operator-facing name
// is the window length, which is what the label actually means. A window whose
// length is unknown (or a per-model bucket) keeps its raw name.
func TestWindowLabelNamesCodexWindowsByLength(t *testing.T) {
	for _, tc := range []struct {
		window Window
		want   string
	}{
		{Window{Name: "primary", WindowMinutes: 300}, "5-hour limit"},
		{Window{Name: "secondary", WindowMinutes: 10080}, "Weekly limit"},
		{Window{Name: "gemini-3.7-flash", WindowMinutes: 0}, "gemini-3.7-flash"},
		{Window{Name: "primary", WindowMinutes: 60}, "primary"},
		{Window{Name: ""}, "window"},
	} {
		if got := tc.window.Label(); got != tc.want {
			t.Errorf("Window%+v.Label() = %q, want %q", tc.window, got, tc.want)
		}
	}
}

func TestHeadlineAvailableShowsRemainingAndReset(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	s := AccountSnapshot{
		Status: StatusAvailable,
		Windows: []Window{
			{Name: "primary", UsedPercent: 7, ResetAt: now.Add(3*time.Hour + 55*time.Minute)},
			{Name: "secondary", UsedPercent: 100, ResetAt: now.Add(11 * time.Hour)},
		},
	}
	got := s.Headline(now)
	if got == "" {
		t.Fatal("empty headline")
	}
	for _, want := range []string{"primary", "93%", "3h"} {
		if !contains(got, want) {
			t.Fatalf("headline %q missing %q", got, want)
		}
	}
}

func TestHeadlineExhaustedNamesReason(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	s := AccountSnapshot{
		Status: StatusExhausted,
		Reason: "claude-opus-4-6-thinking",
		Windows: []Window{
			{Name: "claude-opus-4-6-thinking", UsedPercent: 100, ResetAt: now.Add(72 * time.Hour)},
		},
	}
	got := s.Headline(now)
	for _, want := range []string{"exhausted", "claude-opus-4-6-thinking"} {
		if !contains(got, want) {
			t.Fatalf("headline %q missing %q", got, want)
		}
	}
}

func TestHeadlineUnavailableShowsReason(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	s := AccountSnapshot{Status: StatusUnavailable, Reason: "upstream status 401"}
	got := s.Headline(now)
	for _, want := range []string{"unavailable", "upstream status 401"} {
		if !contains(got, want) {
			t.Fatalf("headline %q missing %q", got, want)
		}
	}
}

func TestHeadlineEscapesNoSecrets(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	s := AccountSnapshot{Status: StatusAvailable, Reason: "Authorization: Bearer sk-secret-token"}
	if got := s.Headline(now); contains(got, "sk-secret-token") {
		t.Fatalf("headline leaked reason content unexpectedly: %q", got)
	}
}

func TestHeadlineUnknownStatusIsStable(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	s := AccountSnapshot{}
	if got := s.Headline(now); got == "" {
		t.Fatal("empty headline for zero status")
	}
}

func TestSortedWindowsOrdersByLowestRemainingWithoutMutatingSnapshot(t *testing.T) {
	s := AccountSnapshot{Windows: []Window{
		{Name: "healthy", UsedPercent: 10},
		{Name: "drained", UsedPercent: 100},
		{Name: "warning", UsedPercent: 80},
		{Name: "same-warning", UsedPercent: 80},
	}}

	got := s.SortedWindows()
	want := []string{"drained", "warning", "same-warning", "healthy"}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("SortedWindows()[%d].Name = %q, want %q", i, got[i].Name, name)
		}
	}
	if s.Windows[0].Name != "healthy" {
		t.Fatalf("SortedWindows mutated snapshot order: %+v", s.Windows)
	}
}

func TestSortedWindowsReturnsIndependentSlice(t *testing.T) {
	s := AccountSnapshot{Windows: []Window{{Name: "first"}, {Name: "second"}}}
	got := s.SortedWindows()
	got[0].Name = "changed"

	if s.Windows[0].Name != "first" {
		t.Fatalf("SortedWindows aliases snapshot storage: %+v", s.Windows)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
