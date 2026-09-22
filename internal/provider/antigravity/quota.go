package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/quota"
)

// quotaPathSuffix is the Cloud Code endpoint the Antigravity CLI uses for its
// /usage view. Unlike retrieveUserQuota, which reports a bucket per model, this
// answers with two model groups (Gemini, Claude+GPT) that each carry a weekly
// and a five-hour window. Those windows are the ones a 429 actually consumes,
// so they are what the dashboard must show.
const quotaPathSuffix = "/v1internal:retrieveUserQuotaSummary"

// quotaBucket is one window of a group in the summary response.
//
// RemainingFraction is a pointer so an absent field is distinguishable from a
// genuine 0: upstream reports 0 only for a drained window, and treating a
// missing field as 0 would mark an account exhausted on malformed data.
type quotaBucket struct {
	BucketID          string   `json:"bucketId"`
	DisplayName       string   `json:"displayName"`
	Window            string   `json:"window"`
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
}

type quotaGroup struct {
	DisplayName string        `json:"displayName"`
	Description string        `json:"description"`
	Buckets     []quotaBucket `json:"buckets"`
}

type quotaResponse struct {
	Groups      []quotaGroup `json:"groups"`
	Description string       `json:"description"`
}

// windowMinutes maps an upstream window label to the operator-facing window
// length. An unrecognized label yields 0, which leaves the raw bucket display
// name as the label rather than inventing a length.
func windowMinutes(window string) int {
	switch window {
	case "weekly":
		return 10080
	case "5h":
		return 300
	default:
		return 0
	}
}

// FetchQuota reports the remaining quota for a single account by calling the
// Antigravity Cloud Code quota-summary endpoint, the same source as the
// Antigravity CLI's usage view.
//
// Classification follows the tri-state contract in internal/quota: only an
// explicit upstream signal (a window at exactly 0 remaining *with* a usable
// reset time) yields StatusExhausted. Everything else that cannot be read is
// StatusUnavailable, which never drains an account, so a failed quota read can
// never black out routing.
func (p *Provider) FetchQuota(ctx context.Context, account provider.Credentials) (quota.AccountSnapshot, error) {
	snap := quota.AccountSnapshot{
		Provider:   p.name,
		Identity:   account.Identity(),
		AccountID:  account.AccountID,
		Email:      account.Email,
		ObservedAt: time.Now(),
	}

	c, err := p.tokenManager(account).EnsureFreshToken(ctx)
	if err != nil {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "token refresh failed"
		return snap, fmt.Errorf("antigravity: quota token: %w", err)
	}

	body := map[string]any{}
	if c.ProjectID != "" {
		body["project"] = c.ProjectID
	}
	resp, err := post(ctx, quotaPathSuffix, c.AccessToken, body)
	if err != nil {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "upstream request failed"
		return snap, fmt.Errorf("antigravity: quota request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snap.Status = quota.StatusUnavailable
		snap.Reason = fmt.Sprintf("upstream status %d", resp.StatusCode)
		return snap, fmt.Errorf("antigravity: quota: upstream status %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "upstream read failed"
		return snap, fmt.Errorf("antigravity: quota read: %w", err)
	}
	var payload quotaResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "malformed quota response"
		return snap, fmt.Errorf("antigravity: quota decode: %w", err)
	}
	snap.Raw = payload

	if len(payload.Groups) == 0 {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "no quota groups"
		return snap, fmt.Errorf("antigravity: quota: no quota groups")
	}

	snap.Groups = make([]quota.Group, 0, len(payload.Groups))
	snap.Windows = make([]quota.Window, 0, 4)
	for _, group := range payload.Groups {
		built := quota.Group{Name: group.DisplayName, Description: group.Description}
		built.Windows = make([]quota.Window, 0, len(group.Buckets))
		for _, b := range group.Buckets {
			if b.RemainingFraction == nil {
				snap.Status = quota.StatusUnavailable
				snap.Reason = fmt.Sprintf("bucket %s has no remaining fraction", bucketLabel(group, b))
				return snap, nil
			}
			remaining := *b.RemainingFraction
			name := b.BucketID
			if name == "" {
				name = group.DisplayName + "-" + b.Window
			}
			window := quota.Window{
				Name:          name,
				Display:       group.DisplayName,
				UsedPercent:   (1 - remaining) * 100,
				WindowMinutes: windowMinutes(b.Window),
			}
			if b.ResetTime != "" {
				if resetAt, err := time.Parse(time.RFC3339, b.ResetTime); err == nil {
					window.ResetAt = resetAt
				}
			}
			if remaining == 0 {
				if window.ResetAt.IsZero() {
					// A drained window with no reset time cannot be
					// distinguished from a malformed read, so report unknown
					// rather than exhausted.
					built.Windows = append(built.Windows, window)
					snap.Windows = append(snap.Windows, window)
					snap.Status = quota.StatusUnavailable
					snap.Reason = fmt.Sprintf("bucket %s exhausted without reset time", bucketLabel(group, b))
					return snap, nil
				}
				if snap.Status != quota.StatusExhausted {
					snap.Status = quota.StatusExhausted
					snap.Reason = window.Label()
				}
			}
			built.Windows = append(built.Windows, window)
			snap.Windows = append(snap.Windows, window)
		}
		snap.Groups = append(snap.Groups, built)
	}

	if snap.Status != quota.StatusExhausted {
		snap.Status = quota.StatusAvailable
	}
	return snap, nil
}

// bucketLabel names a bucket for operator-facing reasons, preferring the
// upstream display name and falling back to the bucket id.
func bucketLabel(group quotaGroup, b quotaBucket) string {
	if b.DisplayName != "" {
		return group.DisplayName + " " + b.DisplayName
	}
	if b.BucketID != "" {
		return group.DisplayName + " " + b.BucketID
	}
	return group.DisplayName
}
