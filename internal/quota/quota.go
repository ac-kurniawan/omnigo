// Package quota holds provider-agnostic quota snapshot types plus the
// in-memory cache shared by the dashboard, the metrics exporter, and the
// opt-in auto-drain syncer.
//
// It deliberately imports nothing from internal/provider: provider
// implementations depend on this package for their snapshot type, so a
// reverse dependency would form an import cycle.
package quota

import (
	"fmt"
	"strings"
	"time"
)

// Status is the tri-state classification of an account's quota.
//
// The third state exists to make accidental blackouts impossible: a failed or
// unparseable quota read is StatusUnavailable, never StatusExhausted, so auto
// drain can require an explicit exhaustion signal.
type Status string

const (
	StatusAvailable   Status = "available"
	StatusExhausted   Status = "exhausted"
	StatusUnavailable Status = "unavailable"
)

// Window is one provider quota window. Codex reports "primary" (5h) and
// "secondary" (7d); Antigravity reports one window per model bucket, so Name
// carries the model id there.
type Window struct {
	Name          string        `json:"name"`
	UsedPercent   float64       `json:"used_percent"`
	WindowMinutes int           `json:"window_minutes,omitempty"`
	ResetAt       time.Time     `json:"reset_at,omitempty"`
	ResetAfter    time.Duration `json:"reset_after,omitempty"`
}

// RemainingPercent is the unconsumed fraction of the window, clamped to
// [0, 100] so a provider reporting >100% used cannot produce a negative
// remaining figure in the UI.
func (w Window) RemainingPercent() float64 {
	remaining := 100 - w.UsedPercent
	if remaining < 0 {
		return 0
	}
	if remaining > 100 {
		return 100
	}
	return remaining
}

// AccountSnapshot is the quota state of a single credential at ObservedAt.
//
// Raw carries the provider-native payload so the dashboard can render full
// upstream detail without this package modelling each provider's schema. It
// must never contain tokens or authorization headers.
type AccountSnapshot struct {
	Provider   string    `json:"provider"`
	Identity   string    `json:"identity"`
	AccountID  string    `json:"account_id,omitempty"`
	Email      string    `json:"email,omitempty"`
	PlanType   string    `json:"plan_type,omitempty"`
	Status     Status    `json:"status"`
	Reason     string    `json:"reason,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	Windows    []Window  `json:"windows,omitempty"`
	Raw        any       `json:"raw,omitempty"`
}

// maxHeadlineWindows caps how many windows the headline lists. Antigravity
// reports one bucket per model, so an uncapped list would be unusable.
const maxHeadlineWindows = 3

// Headline renders a one-line summary: status, per-window remaining percent,
// and the nearest reset. now is a parameter rather than time.Now() so the
// countdown is deterministic in tests.
func (s AccountSnapshot) Headline(now time.Time) string {
	switch s.Status {
	case StatusAvailable:
		if len(s.Windows) == 0 {
			return "available"
		}
		parts := make([]string, 0, maxHeadlineWindows+1)
		for i, w := range s.Windows {
			if i == maxHeadlineWindows {
				parts = append(parts, fmt.Sprintf("+%d more", len(s.Windows)-maxHeadlineWindows))
				break
			}
			parts = append(parts, fmt.Sprintf("%s %s", windowName(w), formatPercent(w.RemainingPercent())))
		}
		summary := s.Status.String() + " (" + strings.Join(parts, " · ")
		if reset := nearestReset(s.Windows, now); reset != "" {
			summary += " · resets in " + reset
		}
		return summary + ")"
	case StatusExhausted:
		summary := s.Status.String()
		if detail := s.exhaustedDetail(now); detail != "" {
			summary += " (" + detail + ")"
		}
		return summary
	case StatusUnavailable:
		if s.Reason == "" {
			return s.Status.String()
		}
		return s.Status.String() + " (" + s.Reason + ")"
	default:
		return "unknown"
	}
}

// String returns the status, or "unknown" for a zero value so templates never
// render an empty badge.
func (s Status) String() string {
	if s == "" {
		return "unknown"
	}
	return string(s)
}

// exhaustedDetail names the binding constraint and when it lifts. The reason
// is operator-supplied text (a model id or a short upstream description), so
// it is safe to display; credentials are never placed in Reason.
func (s AccountSnapshot) exhaustedDetail(now time.Time) string {
	name := ""
	if s.Reason != "" {
		name = s.Reason
	} else {
		for _, w := range s.Windows {
			if w.UsedPercent >= 100 {
				name = windowName(w)
				break
			}
		}
	}
	if name == "" {
		return ""
	}
	for _, w := range s.Windows {
		if windowName(w) != name {
			continue
		}
		if remaining := humanDuration(w.ResetAt.Sub(now)); remaining != "" {
			return name + " resets in " + remaining
		}
	}
	return name
}

func windowName(w Window) string {
	if w.Name == "" {
		return "window"
	}
	return w.Name
}

// nearestReset returns the shortest reset countdown in windows, or "" when no
// window carries a future reset.
func nearestReset(windows []Window, now time.Time) string {
	best := time.Duration(0)
	for _, w := range windows {
		if w.ResetAt.IsZero() {
			continue
		}
		remaining := w.ResetAt.Sub(now)
		if remaining <= 0 {
			continue
		}
		if best == 0 || remaining < best {
			best = remaining
		}
	}
	return humanDuration(best)
}

// humanDuration formats a countdown compactly: "45m", "3h55m", "3d4h".
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	d = d.Round(time.Minute)
	if d < time.Minute {
		d = time.Minute
	}
	days := int(d / (24 * time.Hour))
	hours := int(d/time.Hour) % 24
	minutes := int(d/time.Minute) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// formatPercent trims a trailing ".0" so whole percentages read as "93%".
func formatPercent(p float64) string {
	if p == float64(int(p)) {
		return fmt.Sprintf("%d%%", int(p))
	}
	return fmt.Sprintf("%.1f%%", p)
}
