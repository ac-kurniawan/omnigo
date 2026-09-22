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

// Codex reports generic primary/secondary windows; the operator-facing name is
// their length. Named provider windows keep their model id alongside the known
// window length so multiple bars remain distinguishable.
func TestWindowLabelNamesKnownWindowsByLength(t *testing.T) {
	for _, tc := range []struct {
		window Window
		want   string
	}{
		{Window{Name: "primary", WindowMinutes: 300}, "5-hour limit"},
		{Window{Name: "gemini-3.7-flash", WindowMinutes: 300}, "gemini-3.7-flash · 5-hour limit"},
		{Window{Name: "Gemini Models", WindowMinutes: 10080}, "Gemini Models · Weekly limit"},
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

// An exhausted provider reports the composed window label as the reason, so the
// countdown lookup must still match that window and surface when it lifts.
func TestHeadlineExhaustedComposedLabelStillShowsReset(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	reset := now.Add(17*time.Hour + 43*time.Minute)
	s := AccountSnapshot{
		Status: StatusExhausted,
		Reason: "Claude and GPT models · Weekly limit",
		Windows: []Window{
			{Name: "3p-weekly", Display: "Claude and GPT models", WindowMinutes: 10080, UsedPercent: 100, ResetAt: reset},
			{Name: "3p-5h", Display: "Claude and GPT models", WindowMinutes: 300, UsedPercent: 0, ResetAt: now.Add(time.Hour)},
		},
	}
	got := s.Headline(now)
	for _, want := range []string{"exhausted", "Weekly limit", "17h"} {
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
