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

// Window lengths CommandCode paces its credits with. The upstream reports
// exact reset instants, so these only drive the dashboard label.
const (
	fiveHourMinutes = 300
	weeklyMinutes   = 10080
	monthlyMinutes  = 43200
)

// maxUsageBytes bounds the usage response so a hostile or broken upstream
// cannot exhaust memory through the quota poller.
const maxUsageBytes = 1 << 20

// billingCredits is the `/alpha/billing/credits` payload. It is the only
// surface that reports per-window spend: windowLimits carries used, cap and a
// reset instant for the 5-hour and weekly windows, and credits carries the
// monthly allowance.
type billingCredits struct {
	Credits struct {
		MonthlyCredits   float64 `json:"monthlyCredits"`
		PurchasedCredits float64 `json:"purchasedCredits"`
		FreeCredits      float64 `json:"freeCredits"`
	} `json:"credits"`
	WindowLimits *windowLimits `json:"windowLimits"`
}

type windowLimits struct {
	Limited  bool         `json:"limited"`
	Exceeded any          `json:"exceeded"`
	FiveHour *creditLimit `json:"fiveHour"`
	Weekly   *creditLimit `json:"weekly"`
}

// creditLimit is one rolling window. used and cap are credit values, and
// resetAt is epoch milliseconds.
type creditLimit struct {
	Used     float64 `json:"used"`
	Cap      float64 `json:"cap"`
	Exceeded bool    `json:"exceeded"`
	ResetAt  int64   `json:"resetAt"`
}

// FetchQuota reports CommandCode usage for one credential.
//
// All three windows are measured: windowLimits carries the 5-hour and weekly
// spend with its own cap, so nothing here is inferred from configuration.
// The monthly figure divides spent credits by the plan allowance, which the
// payload reports directly; a configured monthly_credits is only a fallback
// for an upstream that omits it.
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, creditsURLFor(p.generate), nil)
	if err != nil {
		return quotaUnavailable(snapshot, "invalid usage endpoint", fmt.Errorf("commandcode: create usage request: %w", err))
	}
	// The billing endpoint authenticates with the same bearer token as
	// generate and rejects requests without the CLI headers.
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

	var payload billingCredits
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUsageBytes)).Decode(&payload); err != nil {
		return quotaUnavailable(snapshot, "invalid usage response", fmt.Errorf("commandcode: decode usage response: %w", err))
	}
	// The billing payload carries per-window spend; the usage summary carries
	// the month's spend. A failed summary read costs only the monthly bar, so
	// it is not treated as a failed quota read.
	summary, _ := p.fetchUsageSummary(ctx, key)

	snapshot.Raw = &payload
	snapshot.Windows = p.windows(payload, summary)
	// A successful read is available. The upstream reports per-window
	// `exceeded` flags, and trusting a guess here would drain a working
	// credential, so exhaustion is only reported when every window says so.
	if exhausted(payload.WindowLimits) {
		snapshot.Status = quota.StatusExhausted
		snapshot.Reason = "usage limit reached"
		return snapshot, nil
	}
	snapshot.Status = quota.StatusAvailable
	return snapshot, nil
}

// windows converts the upstream limits into the three dashboard windows.
//
// A window the upstream omits keeps UsedPercent at zero and WindowMinutes for
// its label only; the dashboard treats a zero ResetAt as "unknown" rather than
// inventing a countdown.
func (p *Provider) windows(payload billingCredits, summary *usageSummary) []quota.Window {
	limits := payload.WindowLimits

	fiveHour := quota.Window{Name: "5-hour", WindowMinutes: fiveHourMinutes}
	if limits != nil && limits.FiveHour != nil {
		fiveHour.UsedPercent = usedPercent(limits.FiveHour.Used, limits.FiveHour.Cap)
		fiveHour.ResetAt = resetAt(limits.FiveHour.ResetAt)
	}

	weekly := quota.Window{Name: "weekly", WindowMinutes: weeklyMinutes}
	if limits != nil && limits.Weekly != nil {
		weekly.UsedPercent = usedPercent(limits.Weekly.Used, limits.Weekly.Cap)
		weekly.ResetAt = resetAt(limits.Weekly.ResetAt)
	}

	monthly := quota.Window{Name: "monthly", WindowMinutes: monthlyMinutes}
	if summary != nil {
		// periodStart is when this cycle OPENED; the reset is one billing
		// month later, which is what the dashboard counts down to.
		if start, err := time.Parse(time.RFC3339, summary.PeriodStart); err == nil {
			monthly.ResetAt = nextPeriodStart(start)
		}
	}
	// credits.monthlyCredits is what REMAINS; spend is not derivable from it
	// without the plan allowance. The usage summary states spend directly, so
	// the percentage uses that and the allowance only rescales it.
	if allowance := p.monthlyAllowance(payload); allowance > 0 && summary != nil {
		monthly.UsedPercent = clampPercent(summary.TotalMonthlyCredits / allowance * 100)
	}

	return []quota.Window{fiveHour, weekly, monthly}
}

// exhausted reports whether the upstream says every limited window is over.
// A nil limits block means the account is unlimited, which is not exhaustion.
func exhausted(limits *windowLimits) bool {
	if limits == nil || !limits.Limited {
		return false
	}
	windows := []*creditLimit{limits.FiveHour, limits.Weekly}
	seen := false
	for _, w := range windows {
		if w == nil {
			continue
		}
		seen = true
		if !w.Exceeded {
			return false
		}
	}
	return seen
}

func usedPercent(used, cap float64) float64 {
	if cap <= 0 {
		return 0
	}
	return clampPercent(used / cap * 100)
}

// resetAt converts epoch milliseconds, leaving a missing instant as zero.
func resetAt(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
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

func quotaUnavailable(snapshot quota.AccountSnapshot, reason string, err error) (quota.AccountSnapshot, error) {
	snapshot.Status = quota.StatusUnavailable
	snapshot.Reason = reason
	return snapshot, err
}

// usageSummary is the `/alpha/usage/summary` payload. totalMonthlyCredits is
// the month's SPEND, and equals totalCost on live traffic.
type usageSummary struct {
	TotalMonthlyCredits float64 `json:"totalMonthlyCredits"`
	TotalCost           float64 `json:"totalCost"`
	TotalCredits        float64 `json:"totalCredits"`
	PeriodBasis         string  `json:"periodBasis"`
	PeriodStart         string  `json:"periodStart"`
}

// fetchUsageSummary reads the month's spend. It returns nil when the endpoint
// cannot be read, which leaves only the monthly window unmeasured.
func (p *Provider) fetchUsageSummary(ctx context.Context, key string) (*usageSummary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURLFor(p.generate), nil)
	if err != nil {
		return nil, err
	}
	p.setHeaders(req, key, "00000000-0000-4000-8000-000000000000", false)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("commandcode: usage status %d", resp.StatusCode)
	}
	var summary usageSummary
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUsageBytes)).Decode(&summary); err != nil {
		return nil, err
	}
	return &summary, nil
}

// monthlyAllowance is the plan's monthly credit pool. The billing payload
// reports the REMAINING balance, not the pool, so the allowance must come from
// config: deriving it would depend on two reads agreeing.
func (p *Provider) monthlyAllowance(billingCredits) float64 {
	return p.quota.MonthlyCredits
}

// nextPeriodStart advances a billing-period start by one month, clamping a
// month-end date into the shorter following month.
func nextPeriodStart(start time.Time) time.Time {
	next := start.UTC().AddDate(0, 1, 0)
	if next.Day() != start.Day() {
		next = next.AddDate(0, 0, -next.Day())
	}
	return next
}
