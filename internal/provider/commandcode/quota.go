package commandcode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/quota"
)

var _ provider.QuotaFetcher = (*Provider)(nil)

// Window lengths CommandCode paces its credits with. The upstream meters
// included *monthly* credits through two rolling windows: a 5-hour burst cap
// and a 7-day cap. Both are documented per plan, not returned by the API.
const (
	fiveHourMinutes = 300
	weeklyMinutes   = 10080
	monthlyMinutes  = 43200
)

// maxUsageBytes bounds the usage response so a hostile or broken upstream
// cannot exhaust memory through the quota poller.
const maxUsageBytes = 1 << 20

// usageSummary is the `/alpha/usage/summary` payload. Every field is optional
// because the endpoint has shipped additions before; a missing number is zero,
// which the windows below treat as "not reported" rather than "none left".
type usageSummary struct {
	TotalCount            int     `json:"totalCount"`
	TotalCost             float64 `json:"totalCost"`
	TotalTokensIn         int64   `json:"totalTokensIn"`
	TotalTokensOut        int64   `json:"totalTokensOut"`
	TotalTokens           int64   `json:"totalTokens"`
	TotalCredits          float64 `json:"totalCredits"`
	TotalMonthlyCredits   float64 `json:"totalMonthlyCredits"`
	TotalPurchasedCredits float64 `json:"totalPurchasedCredits"`
	PeriodBasis           string  `json:"periodBasis"`
	PeriodStart           string  `json:"periodStart"`
}

// FetchQuota reports CommandCode usage for one credential.
//
// Only the monthly figure is measured upstream. The API exposes no 5-hour or
// weekly spend, and no window caps at all, so those windows carry their
// configured length and a zero UsedPercent: the dashboard shows the caps from
// the operator's config rather than a percentage invented here.
//
// A quota read must never drain an account, so every failure is a plain error
// and StatusUnavailable — never provider.NewHTTPStatusError.
func (p *Provider) FetchQuota(ctx context.Context, account provider.Credentials) (quota.AccountSnapshot, error) {
	snapshot := quota.AccountSnapshot{
		Provider:   p.name,
		Identity:   account.Identity(),
		AccountID:  account.AccountID,
		Email:      account.Email,
		ObservedAt: time.Now(),
	}

	key := account.APIKey
	if key == "" {
		key = account.AccessToken
	}
	if key == "" {
		return quotaUnavailable(snapshot, "no api key configured", fmt.Errorf("commandcode: no credentials"))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURLFor(p.generate), nil)
	if err != nil {
		return quotaUnavailable(snapshot, "invalid usage endpoint", fmt.Errorf("commandcode: create usage request: %w", err))
	}
	// The usage endpoint authenticates with the same bearer token as generate
	// and rejects requests without the CLI headers.
	p.setHeaders(req, key, "00000000-0000-4000-8000-000000000000", false)

	resp, err := p.client.Do(req)
	if err != nil {
		return quotaUnavailable(snapshot, "upstream unreachable", fmt.Errorf("commandcode: usage request failed: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		reason := fmt.Sprintf("upstream status %d", resp.StatusCode)
		return quotaUnavailable(snapshot, reason, fmt.Errorf("commandcode: quota %s", reason))
	}

	var summary usageSummary
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUsageBytes)).Decode(&summary); err != nil {
		return quotaUnavailable(snapshot, "invalid usage response", fmt.Errorf("commandcode: decode usage response: %w", err))
	}

	snapshot.PlanType = summary.PeriodBasis
	snapshot.Raw = &summary
	snapshot.Windows = p.windows(summary)

	// A successful read is available. The API reports spend but never an
	// exhaustion signal, so this must not guess StatusExhausted: a wrong
	// exhaustion drains a working credential.
	snapshot.Status = quota.StatusAvailable
	return snapshot, nil
}

// windows builds the three windows CommandCode paces usage with. Monthly is
// measured; the 5-hour and weekly caps are not metered upstream, so their
// allowance is what the operator configured, with no percentage.
func (p *Provider) windows(summary usageSummary) []quota.Window {
	monthly := quota.Window{
		Name:          "monthly",
		WindowMinutes: monthlyMinutes,
	}
	if p.quota.MonthlyCredits > 0 {
		monthly.UsedPercent = clampPercent(summary.TotalMonthlyCredits / p.quota.MonthlyCredits * 100)
	}
	monthly.ResetAt = nextPeriodStart(summary.PeriodStart)

	return []quota.Window{
		{
			Name:          "5-hour",
			Display:       creditAllowance(p.quota.FiveHourCap),
			WindowMinutes: fiveHourMinutes,
		},
		{
			Name:          "weekly",
			Display:       creditAllowance(p.quota.WeeklyCap),
			WindowMinutes: weeklyMinutes,
		},
		monthly,
	}
}

// creditAllowance renders a configured window cap so the dashboard can show
// the operator's plan limit. It returns "" when no cap is configured, which
// leaves the window unlabelled rather than showing a misleading zero.
func creditAllowance(cap float64) string {
	if cap <= 0 {
		return ""
	}
	return fmt.Sprintf("cap %g credits", cap)
}

func clampPercent(value float64) float64 {
	// NaN fails both comparisons, so it would survive a plain clamp and reach
	// the metrics label and progress bar as a poisoned sample.
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// nextPeriodStart turns the period's start into the boundary it resets at.
// The API reports only when the current billing period opened, and the UI
// renders a past ResetAt as "now", so leaving the start there would be wrong.
// A billing period renews on the same day of the month, and an unparseable
// timestamp stays zero so the dashboard shows no countdown at all.
func nextPeriodStart(raw string) time.Time {
	start, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	next := start.UTC().AddDate(0, 1, 0)
	// A month-end start (Jan 31) would roll past the shorter month; clamp back
	// to its last day so the boundary stays inside the following month.
	if next.Day() != start.Day() {
		next = next.AddDate(0, 0, -next.Day())
	}
	return next
}

func quotaUnavailable(snapshot quota.AccountSnapshot, reason string, err error) (quota.AccountSnapshot, error) {
	snapshot.Status = quota.StatusUnavailable
	snapshot.Reason = reason
	return snapshot, err
}
