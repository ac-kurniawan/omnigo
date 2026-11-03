package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/quota"
)

// quotaPathSuffix is the Cloud Code endpoint the Antigravity CLI uses for its
// /usage view: groups (Claude+GPT, Gemini) that each carry a weekly and a
// five-hour window, the limits a 429 actually consumes.
//
// It only reports a group whose limit is in force, so an account with Claude
// drained answers with the Claude group alone even while Gemini has headroom.
// quotaModelPathSuffix covers that gap: one bucket per model, regardless of
// which group is binding.
const (
	quotaPathSuffix      = "/v1internal:retrieveUserQuotaSummary"
	quotaModelPathSuffix = "/v1internal:retrieveUserQuota"
)

// geminiGroupName heads the synthesized group built from per-model buckets, so
// it reads like the upstream "Claude and GPT models" heading.
const geminiGroupName = "Gemini Models"

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

// quotaModelBucket is one per-model entry of retrieveUserQuota.
type quotaModelBucket struct {
	ModelID           string   `json:"modelId"`
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
}

type quotaModelResponse struct {
	Buckets []quotaModelBucket `json:"buckets"`
}

// geminiMalformed reports whether any per-model Gemini bucket omitted its
// remaining fraction, so the caller can downgrade the read to unavailable
// instead of rendering a bucket as either drained or fresh.
func (r quotaModelResponse) geminiMalformed() (string, bool) {
	for _, b := range r.Buckets {
		if strings.HasPrefix(b.ModelID, "gemini-") && b.RemainingFraction == nil {
			return b.ModelID, true
		}
	}
	return "", false
}

// Window lengths, in minutes, mirroring the labels the summary reports. They
// also drive the label quota.Window.Label() renders.
const (
	fiveHourMinutes = 300
	weeklyMinutes   = 10080
)

// fiveHour is the burst window. A per-model reset within it is that window;
// anything later is the weekly cap.
const fiveHour = fiveHourMinutes * time.Minute

// timeNow is a var so tests can pin the clock when classifying reset horizons.
var timeNow = time.Now

// windowMinutes maps an upstream window label to the operator-facing window
// length. An unrecognized label yields 0, which leaves the raw bucket display
// name as the label rather than inventing a length.
func windowMinutes(window string) int {
	switch window {
	case "weekly":
		return weeklyMinutes
	case "5h":
		return fiveHourMinutes
	default:
		return 0
	}
}

// FetchQuota reports the remaining quota for a single account from the two
// Cloud Code endpoints that back the Antigravity CLI's usage view.
//
// The summary endpoint names the groups and their shared windows. The per-model
// endpoint is merged in as an extra Gemini group whenever the summary omits
// one, because the summary reports only the groups with a limit in force: a
// drained Claude weekly limit hides Gemini headroom that still exists.
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

	raw, err := postQuota(ctx, quotaPathSuffix, c.AccessToken, body)
	if err != nil {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "upstream request failed"
		var status quotaStatusError
		if errors.As(err, &status) {
			snap.Reason = status.Error()
		}
		return snap, fmt.Errorf("antigravity: quota request: %w", err)
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
				markExhausted(&snap, window.Label())
			}
			built.Windows = append(built.Windows, window)
			snap.Windows = append(snap.Windows, window)
		}
		snap.Groups = append(snap.Groups, built)
	}

	// The per-model read is additive detail: a failure here leaves the summary
	// groups intact rather than discarding a good read.
	if !hasGroupNamed(snap.Groups, geminiGroupName) {
		if raw, err := postQuota(ctx, quotaModelPathSuffix, c.AccessToken, body); err == nil {
			var models quotaModelResponse
			if json.Unmarshal(raw, &models) == nil {
				if model, bad := models.geminiMalformed(); bad {
					snap.Status = quota.StatusUnavailable
					snap.Reason = fmt.Sprintf("bucket %s has no remaining fraction", model)
					return snap, nil
				}
				if gem := geminiGroup(models.Buckets, timeNow()); len(gem.Windows) > 0 {
					// Gemini leads, matching the CLI's usage view, where the
					// first-party group is listed before third-party models.
					snap.Groups = append([]quota.Group{gem}, snap.Groups...)
					snap.Windows = append(gem.Windows, snap.Windows...)
					for _, w := range gem.Windows {
						if w.RemainingPercent() == 0 && !w.ResetAt.IsZero() {
							markExhausted(&snap, w.Label())
						}
					}
				}
			}
		}
	}

	if snap.Status != quota.StatusExhausted && snap.Status != quota.StatusUnavailable {
		snap.Status = quota.StatusAvailable
	}
	return snap, nil
}

// quotaStatusError marks a non-200 quota read, keeping the status operator
// facing without leaking response bodies.
type quotaStatusError struct{ code int }

func (e quotaStatusError) Error() string { return fmt.Sprintf("upstream status %d", e.code) }

// postQuota performs one quota call and returns the 200 body.
func postQuota(ctx context.Context, path, accessToken string, body map[string]any) ([]byte, error) {
	resp, err := post(ctx, path, accessToken, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, quotaStatusError{code: resp.StatusCode}
	}
	return io.ReadAll(resp.Body)
}

// markExhausted records the first exhausted window as the account's reason. A
// later one must not overwrite it: the reason names the constraint the
// operator should act on, and windows are visited in upstream order.
func markExhausted(snap *quota.AccountSnapshot, reason string) {
	if snap.Status == quota.StatusExhausted || snap.Status == quota.StatusUnavailable {
		return
	}
	snap.Status = quota.StatusExhausted
	snap.Reason = reason
}

func hasGroupNamed(groups []quota.Group, name string) bool {
	for _, g := range groups {
		if g.Name == name {
			return true
		}
	}
	return false
}

// geminiGroup collapses the per-model buckets into one Gemini group with a
// weekly and a five-hour bar, each holding the lowest remaining fraction among
// the models that report it.
//
// The group shares one quota, so the model with the least left is the one that
// binds first, and it is usually the model actually in use.
//
// The per-model endpoint carries no window field, so the reset horizon decides
// the bar: a reset inside the five-hour burst period is that window, anything
// later is the weekly cap. A bucket with no reset time cannot be attributed and
// is skipped rather than guessed at.
//
// Only Gemini ids are kept: the endpoint also reports Claude, GPT and internal
// ids whose quota the summary groups already own.
func geminiGroup(buckets []quotaModelBucket, now time.Time) quota.Group {
	group := quota.Group{Name: geminiGroupName}
	var binding [2]*quota.Window
	for _, b := range buckets {
		if !strings.HasPrefix(b.ModelID, "gemini-") || b.RemainingFraction == nil || b.ResetTime == "" {
			continue
		}
		resetAt, err := time.Parse(time.RFC3339, b.ResetTime)
		if err != nil {
			continue
		}
		window := quota.Window{
			Name:        geminiGroupName,
			Display:     geminiGroupName,
			UsedPercent: (1 - *b.RemainingFraction) * 100,
			ResetAt:     resetAt,
		}
		slot := 0
		if resetAt.Sub(now) <= fiveHour {
			slot = 1
			window.WindowMinutes = fiveHourMinutes
		} else {
			window.WindowMinutes = weeklyMinutes
		}
		if binding[slot] == nil || window.RemainingPercent() < binding[slot].RemainingPercent() {
			binding[slot] = &window
		}
	}
	// Weekly first, matching the order the summary lists its own windows in.
	for _, w := range binding {
		if w != nil {
			group.Windows = append(group.Windows, *w)
		}
	}
	return group
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

// MarkQuotaDrained cools one model on one credential in this provider's
// account pool because that model's upstream quota is exhausted. An empty
// model cools the whole account. It lets the syncer pre-drain through the
// same pool the request path uses, without exposing the pool itself.
func (p *Provider) MarkQuotaDrained(identity, model string, cooldown time.Duration, reason string) {
	p.pool.MarkQuotaDrained(identity, model, cooldown, reason)
}
