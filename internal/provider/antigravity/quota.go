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

// quotaPathSuffix is the Cloud Code endpoint that reports per-model remaining
// quota. It is POSTed with the resolved GCP project id, and answers with one
// bucket per model the account can call.
const quotaPathSuffix = "/v1internal:retrieveUserQuota"

// quotaBucket is one model-scoped bucket of the retrieveUserQuota response.
//
// RemainingFraction is a pointer so an absent field is distinguishable from a
// genuine 0: upstream reports 0 only for a drained model, and treating a
// missing field as 0 would drain an account on malformed data.
type quotaBucket struct {
	TokenType         string   `json:"tokenType"`
	ModelID           string   `json:"modelId"`
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
}

type quotaResponse struct {
	Buckets []quotaBucket `json:"buckets"`
}

// FetchQuota reports the remaining quota for a single account by calling the
// Antigravity Cloud Code quota endpoint.
//
// Classification follows the tri-state contract in internal/quota: only an
// explicit upstream signal (a bucket at exactly 0 remaining *with* a usable
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

	if len(payload.Buckets) == 0 {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "no quota buckets"
		return snap, fmt.Errorf("antigravity: quota: no quota buckets")
	}

	snap.Windows = make([]quota.Window, 0, len(payload.Buckets))
	for _, b := range payload.Buckets {
		if b.RemainingFraction == nil {
			snap.Status = quota.StatusUnavailable
			snap.Reason = fmt.Sprintf("bucket %s has no remaining fraction", b.ModelID)
			return snap, nil
		}
		remaining := *b.RemainingFraction
		window := quota.Window{
			Name:        b.ModelID,
			UsedPercent: (1 - remaining) * 100,
		}
		if b.ResetTime != "" {
			if resetAt, err := time.Parse(time.RFC3339, b.ResetTime); err == nil {
				window.ResetAt = resetAt
			}
		}
		if remaining == 0 {
			if window.ResetAt.IsZero() {
				// A drained bucket with no reset time cannot be distinguished
				// from a malformed read, so report unknown rather than exhausted.
				snap.Windows = append(snap.Windows, window)
				snap.Status = quota.StatusUnavailable
				snap.Reason = fmt.Sprintf("bucket %s exhausted without reset time", b.ModelID)
				return snap, nil
			}
			if snap.Status != quota.StatusExhausted {
				snap.Status = quota.StatusExhausted
				snap.Reason = b.ModelID
			}
		}
		snap.Windows = append(snap.Windows, window)
	}

	if snap.Status != quota.StatusExhausted {
		snap.Status = quota.StatusAvailable
	}
	return snap, nil
}
