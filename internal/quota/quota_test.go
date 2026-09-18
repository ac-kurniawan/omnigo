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
