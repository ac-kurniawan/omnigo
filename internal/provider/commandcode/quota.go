package commandcode

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

// goPlanMonthlyCap is the $1 Go plan's monthly credit allowance. Non-Go plans
// report their own remaining balance; the cap only bounds the monthly window
// for the Go plan label.
const goPlanMonthlyCap = 10.0

type whoamiLimits struct {
	FiveHour *ccLimit `json:"fiveHour"`
	Weekly   *ccLimit `json:"weekly"`
}

type ccLimit struct {
	Used      float64 `json:"used"`
	Limit     float64 `json:"limit"`
	Remaining float64 `json:"remaining"`
	ResetAt   string  `json:"resetAt"`
	Reset     string  `json:"reset"`
}

type whoamiPayload struct {
	Org struct {
		ID     string `json:"id"`
		Plan   string `json:"plan"`
		PlanID string `json:"planId"`
	} `json:"org"`
	UserID       string        `json:"userId"`
	Email        string        `json:"email"`
	Limits       *whoamiLimits `json:"limits"`
	WindowLimits *whoamiLimits `json:"windowLimits"`
	Credits      *struct {
		Remaining float64 `json:"remaining"`
		Used      float64 `json:"used"`
		Limit     float64 `json:"limit"`
		Balance   float64 `json:"balance"`
		Plan      string  `json:"plan"`
		PlanID    string  `json:"planId"`
	} `json:"credits"`
	Subscriptions *struct {
		Plan   string `json:"plan"`
		PlanID string `json:"planId"`
		Status string `json:"status"`
	} `json:"subscriptions"`
	Raw map[string]any `json:"-"`
}

// FetchQuota reports remaining quota for one account. It resolves the org
// scope from the stored ProjectID, falling back to a whoami lookup, then
// reads credits and subscriptions. Missing fields yield a partial snapshot,
// never a panic; only explicit exhaustion signals yield StatusExhausted.
func (p *Provider) FetchQuota(ctx context.Context, account provider.Credentials) (quota.AccountSnapshot, error) {
	snap := quota.AccountSnapshot{
		Provider:   p.name,
		Identity:   account.Identity(),
		AccountID:  account.AccountID,
		Email:      account.Email,
		ObservedAt: time.Now(),
		Status:     quota.StatusAvailable,
	}
	key := apiKey(account)
	if key == "" {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "not authenticated"
		return snap, fmt.Errorf("commandcode: not authenticated")
	}
	orgID := strings.TrimSpace(account.ProjectID)
	payload, err := p.fetchWhoami(ctx, key)
	if err != nil {
		if isAuthFailure(err) {
			snap.Status = quota.StatusUnavailable
			snap.Reason = "auth failed"
			return snap, err
		}
		snap.Status = quota.StatusUnavailable
		snap.Reason = "whoami failed"
		return snap, err
	}
	if orgID == "" {
		orgID = payload.Org.ID
	}
	if payload.Org.PlanID != "" {
		snap.PlanType = payload.Org.PlanID
	} else if payload.Org.Plan != "" {
		snap.PlanType = payload.Org.Plan
	}
	snap.Raw = map[string]any{"plan": snap.PlanType, "org": orgID}
	if payload.Credits != nil {
		snap.Raw = map[string]any{
			"plan":      snap.PlanType,
			"org":       orgID,
			"remaining": payload.Credits.Remaining,
			"balance":   payload.Credits.Balance,
		}
	}
	limits := payload.WindowLimits
	if limits == nil {
		limits = payload.Limits
	}
	snap.Windows = buildQuotaWindows(payload.Credits, limits, snap.PlanType)
	for _, w := range snap.Windows {
		if w.UsedPercent >= 100 && !w.ResetAt.IsZero() && time.Now().Before(w.ResetAt) {
			snap.Status = quota.StatusExhausted
			snap.Reason = w.Label()
			break
		}
	}
	return snap, nil
}

func buildQuotaWindows(credits *struct {
	Remaining float64 `json:"remaining"`
	Used      float64 `json:"used"`
	Limit     float64 `json:"limit"`
	Balance   float64 `json:"balance"`
	Plan      string  `json:"plan"`
	PlanID    string  `json:"planId"`
}, limits *whoamiLimits, plan string) []quota.Window {
	windows := []quota.Window{}
	if credits != nil {
		limit := credits.Limit
		if limit <= 0 && strings.EqualFold(plan, "go") {
			limit = goPlanMonthlyCap
		}
		if limit <= 0 {
			limit = credits.Remaining + credits.Used
		}
		if limit > 0 {
			used := credits.Used
			if used <= 0 && credits.Remaining > 0 {
				used = limit - credits.Remaining
			}
			usedPct := 0.0
			if limit > 0 {
				usedPct = used / limit * 100
			}
			windows = append(windows, quota.Window{
				Name: "monthly", Display: "Monthly credits",
				UsedPercent: usedPct,
			})
		}
	}
	if limits != nil {
		if limits.FiveHour != nil {
			windows = append(windows, limitWindow("session", "Session (5h)", 300, limits.FiveHour))
		}
		if limits.Weekly != nil {
			windows = append(windows, limitWindow("weekly", "Weekly", 10080, limits.Weekly))
		}
	}
	return windows
}

func limitWindow(name, display string, minutes int, l *ccLimit) quota.Window {
	w := quota.Window{Name: name, Display: display, WindowMinutes: minutes}
	total := l.Limit
	if total <= 0 {
		total = l.Used + l.Remaining
	}
	if total > 0 {
		used := l.Used
		if used <= 0 && l.Remaining > 0 {
			used = total - l.Remaining
		}
		w.UsedPercent = used / total * 100
	}
	if ts := l.ResetAt; ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			w.ResetAt = t
		}
	} else if ts := l.Reset; ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			w.ResetAt = t
		}
	}
	return w
}

func (p *Provider) fetchWhoami(ctx context.Context, key string) (*whoamiPayload, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase+"/alpha/whoami?limits=1", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range buildCommandCodeHeaders(key, "", "application/json") {
		req.Header[k] = v
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, provider.NewHTTPStatusError(resp.StatusCode, "commandcode: auth failed")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("commandcode: whoami status %d", resp.StatusCode))
	}
	var payload whoamiPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("commandcode: decode whoami: %w", err)
	}
	return &payload, nil
}

func isAuthFailure(err error) bool {
	var he *provider.HTTPStatusError
	if !errors.As(err, &he) {
		return false
	}
	return he.StatusCode == http.StatusUnauthorized || he.StatusCode == http.StatusForbidden
}
