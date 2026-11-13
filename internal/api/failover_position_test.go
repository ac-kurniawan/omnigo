package api

import "testing"

// The failover decision is positional, not identity-based: a provider/model can
// legitimately be listed twice in one chain, and only the dispatch that has
// nothing after it may forward a stream instead of buffering for a failover.
func TestHasTargetAfterCountsDispatches(t *testing.T) {
	cases := []struct {
		name       string
		total      int
		dispatched int
		want       bool
	}{
		{"single target", 1, 0, false},
		{"first of two", 2, 0, true},
		{"last of two", 2, 1, false},
		{"duplicate first of [a,a]", 2, 0, true},
		{"duplicate last of [a,a]", 2, 1, false},
		{"first of [a,b,a]", 3, 0, true},
		{"middle of [a,b,a]", 3, 1, true},
		{"final of [a,b,a]", 3, 2, false},
		{"empty chain", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasTargetAfter(tc.total, tc.dispatched); got != tc.want {
				t.Fatalf("hasTargetAfter(%d, %d) = %v, want %v", tc.total, tc.dispatched, got, tc.want)
			}
		})
	}
}
