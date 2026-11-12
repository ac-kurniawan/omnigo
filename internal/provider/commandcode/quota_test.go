package commandcode

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/quota"
)

// billingBody mirrors a real /alpha/billing/credits response: windowLimits is
// the only place per-window spend appears.
const billingBody = `{
  "credits": {"belowThreshold": false, "monthlyCredits": 9.4932205459, "purchasedCredits": 0, "freeCredits": 0},
  "windowLimits": {
    "limited": true,
    "exceeded": null,
    "fiveHour": {"used": 0.023995582, "cap": 3, "exceeded": false, "resetAt": 1794540754769},
    "weekly": {"used": 0.4858360528, "cap": 6, "exceeded": false, "resetAt": 1795098681945}
  },
  "sandboxAccess": false
}`

const summaryBody = `{"totalMonthlyCredits":0.5067794541,"totalCost":0.5067794541,
 "totalCredits":0.5067794541,"periodBasis":"billing-period","periodStart":"2026-10-26T13:40:58.000Z"}`

// billingServer serves both routes the fetch reads: the billing payload for
// per-window spend and the summary for monthly spend.
func billingServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload string
		switch r.URL.Path {
		case "/alpha/billing/credits":
			payload = body
		case "/alpha/usage/summary":
			payload = summaryBody
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
}

func fetchQuota(t *testing.T, srv *httptest.Server, cfg provider.Config, key string) quota.AccountSnapshot {
	t.Helper()
	cfg.Name = "cc"
	cfg.BaseURL = srv.URL
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	p := New(cfg, &memoryCredStore{creds: provider.Credentials{APIKey: key}})
	snap, err := p.(provider.QuotaFetcher).FetchQuota(context.Background(), provider.Credentials{APIKey: key})
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	return snap
}

func windowByName(t *testing.T, snap quota.AccountSnapshot, name string) quota.Window {
	t.Helper()
	for _, w := range snap.Windows {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("missing %s window, got %+v", name, snap.Windows)
	return quota.Window{}
}

func TestCommandCodeFetchQuotaMeasuresEveryWindow(t *testing.T) {
	ts := billingServer(t, billingBody)
	defer ts.Close()

	snap := fetchQuota(t, ts, provider.Config{}, "user_ok")
	if snap.Status != quota.StatusAvailable {
		t.Fatalf("Status = %q, want available", snap.Status)
	}
	if snap.Identity == "user_ok" {
		t.Error("Identity leaked the API key")
	}

	// These are the values the dashboard renders. A hardcoded zero here is
	// what made every window read "100% remaining".
	fiveHour := windowByName(t, snap, "5-hour")
	if got, want := fiveHour.UsedPercent, 0.023995582/3*100; math.Abs(got-want) > 1e-9 {
		t.Errorf("5-hour UsedPercent = %v, want %v", got, want)
	}
	if fiveHour.RemainingPercent() < 99 {
		t.Errorf("5-hour remaining = %v, want ~99.2", fiveHour.RemainingPercent())
	}
	weekly := windowByName(t, snap, "weekly")
	if got, want := weekly.UsedPercent, 0.4858360528/6*100; math.Abs(got-want) > 1e-9 {
		t.Errorf("weekly UsedPercent = %v, want %v", got, want)
	}

	// resetAt is epoch milliseconds upstream.
	if want := time.UnixMilli(1794540754769).UTC(); !fiveHour.ResetAt.Equal(want) {
		t.Errorf("5-hour ResetAt = %v, want %v", fiveHour.ResetAt, want)
	}
	if fiveHour.WindowMinutes != 300 || weekly.WindowMinutes != 10080 {
		t.Errorf("window lengths = %d/%d", fiveHour.WindowMinutes, weekly.WindowMinutes)
	}
}

func TestCommandCodeFetchQuotaMonthlySpend(t *testing.T) {
	ts := billingServer(t, billingBody)
	defer ts.Close()

	snap := fetchQuota(t, ts, provider.Config{Quota: provider.QuotaConfig{MonthlyCredits: 10}}, "user_ok")
	monthly := windowByName(t, snap, "monthly")
	// Spend comes from the usage summary, rescaled by the plan allowance.
	want := 0.5067794541 / 10 * 100
	if math.Abs(monthly.UsedPercent-want) > 1e-9 {
		t.Fatalf("monthly UsedPercent = %v, want %v", monthly.UsedPercent, want)
	}
}

func TestCommandCodeFetchQuotaUsesWindowCaps(t *testing.T) {
	// The configured caps must not override what upstream reports.
	ts := billingServer(t, billingBody)
	defer ts.Close()

	snap := fetchQuota(t, ts, provider.Config{Quota: provider.QuotaConfig{FiveHourCap: 999, WeeklyCap: 999}}, "user_ok")
	if got := windowByName(t, snap, "5-hour").UsedPercent; got < 0.7 || got > 0.9 {
		t.Fatalf("5-hour UsedPercent = %v, want ~0.8 from the upstream cap", got)
	}
}

func TestCommandCodeFetchQuotaExhaustedOnlyWhenEveryWindowExceeded(t *testing.T) {
	both := `{"credits":{"monthlyCredits":5},"windowLimits":{"limited":true,
		"fiveHour":{"used":3,"cap":3,"exceeded":true,"resetAt":1794540754769},
		"weekly":{"used":6,"cap":6,"exceeded":true,"resetAt":1795098681945}}}`
	ts := billingServer(t, both)
	defer ts.Close()
	if got := fetchQuota(t, ts, provider.Config{}, "user_ok").Status; got != quota.StatusExhausted {
		t.Fatalf("Status = %q, want exhausted", got)
	}

	// One window under its cap is a working account, not an exhausted one.
	one := `{"credits":{"monthlyCredits":5},"windowLimits":{"limited":true,
		"fiveHour":{"used":3,"cap":3,"exceeded":true,"resetAt":1794540754769},
		"weekly":{"used":1,"cap":6,"exceeded":false,"resetAt":1795098681945}}}`
	ts2 := billingServer(t, one)
	defer ts2.Close()
	if got := fetchQuota(t, ts2, provider.Config{}, "user_ok").Status; got != quota.StatusAvailable {
		t.Fatalf("Status = %q, want available when a window has headroom", got)
	}
}

func TestCommandCodeFetchQuotaUnavailableNeverDrains(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer ts.Close()

	p := New(provider.Config{Name: "cc", BaseURL: ts.URL, Timeout: 5 * time.Second},
		&memoryCredStore{creds: provider.Credentials{APIKey: "user_bad"}})
	snap, err := p.(provider.QuotaFetcher).FetchQuota(context.Background(), provider.Credentials{APIKey: "user_bad"})
	if err == nil {
		t.Fatal("expected an error for a rejected key")
	}
	if snap.Status != quota.StatusUnavailable {
		t.Fatalf("Status = %q, want unavailable", snap.Status)
	}
	var statusErr *provider.HTTPStatusError
	if errors.As(err, &statusErr) {
		t.Fatal("quota errors must not be HTTPStatusError, which the pool could treat as drainable")
	}
	if strings.Contains(snap.Reason, "invalid api key") {
		t.Error("Reason leaked the upstream body")
	}
}

func TestCommandCodeFetchQuotaMissingKey(t *testing.T) {
	p := New(provider.Config{Name: "cc", Timeout: 5 * time.Second}, &memoryCredStore{})
	snap, err := p.(provider.QuotaFetcher).FetchQuota(context.Background(), provider.Credentials{})
	if err == nil {
		t.Fatal("expected an error with no credentials")
	}
	if snap.Status != quota.StatusUnavailable {
		t.Fatalf("Status = %q, want unavailable", snap.Status)
	}
}

func TestCommandCodeFetchQuotaMissingWindowLimits(t *testing.T) {
	// A pay-as-you-go account has no windows; it must not be called exhausted.
	ts := billingServer(t, `{"credits":{"monthlyCredits":4.5},"windowLimits":null}`)
	defer ts.Close()
	snap := fetchQuota(t, ts, provider.Config{}, "user_ok")
	if snap.Status != quota.StatusAvailable {
		t.Fatalf("Status = %q, want available", snap.Status)
	}
	for _, name := range []string{"5-hour", "weekly"} {
		if w := windowByName(t, snap, name); w.UsedPercent != 0 || !w.ResetAt.IsZero() {
			t.Errorf("%s window = %+v, want no data", name, w)
		}
	}
}

func TestClampPercentRejectsNonFinite(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0},
		{-5, 0}, {150, 100}, {42.5, 42.5},
	}
	for _, c := range cases {
		if got := clampPercent(c.in); got != c.want {
			t.Errorf("clampPercent(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCommandCodeFetchQuotaRawIsJSON(t *testing.T) {
	ts := billingServer(t, billingBody)
	defer ts.Close()
	snap := fetchQuota(t, ts, provider.Config{}, "user_ok")
	encoded, err := json.Marshal(snap.Raw)
	if err != nil {
		t.Fatalf("Raw is not JSON: %v", err)
	}
	if strings.Contains(string(encoded), "user_ok") {
		t.Fatal("Raw leaked the API key")
	}
}

func TestNextPeriodStartClampsMonthEnd(t *testing.T) {
	cases := []struct{ start, want string }{
		{"2026-10-26T13:40:58.000Z", "2026-11-26T13:40:58Z"},
		{"2026-01-31T00:00:00Z", "2026-02-28T00:00:00Z"},
		{"2026-12-31T05:00:00Z", "2027-01-31T05:00:00Z"},
	}
	for _, c := range cases {
		start, _ := time.Parse(time.RFC3339, c.start)
		want, _ := time.Parse(time.RFC3339, c.want)
		if got := nextPeriodStart(start); !got.Equal(want) {
			t.Errorf("nextPeriodStart(%s) = %v, want %v", c.start, got, want)
		}
	}
}

func TestCommandCodeFetchQuotaMonthlyResetIsNextCycle(t *testing.T) {
	ts := billingServer(t, billingBody)
	defer ts.Close()
	snap := fetchQuota(t, ts, provider.Config{Quota: provider.QuotaConfig{MonthlyCredits: 10}}, "user_ok")
	monthly := windowByName(t, snap, "monthly")
	want := time.Date(2026, 11, 26, 13, 40, 58, 0, time.UTC)
	if !monthly.ResetAt.Equal(want) {
		t.Fatalf("monthly ResetAt = %v, want next cycle %v", monthly.ResetAt, want)
	}
}
