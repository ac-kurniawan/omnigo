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

func usageServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alpha/usage/summary" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

const usageBody = `{
  "totalCount": 157,
  "totalCost": 0.4827838721,
  "totalTokensIn": 17888707,
  "totalTokensOut": 92115,
  "totalTokens": 17980822,
  "totalCredits": 0.4827838721,
  "totalMonthlyCredits": 0.4827838721,
  "totalPurchasedCredits": 0,
  "periodBasis": "billing-period",
  "periodStart": "2026-10-26T13:40:58.000Z"
}`

func TestCommandCodeFetchQuotaReportsMonthlyUsage(t *testing.T) {
	ts := usageServer(t, usageBody)
	defer ts.Close()

	p := New(provider.Config{Name: "cmc", BaseURL: ts.URL, Timeout: 5 * time.Second},
		&memoryCredStore{creds: provider.Credentials{APIKey: "user_ok"}})

	snap, err := p.(provider.QuotaFetcher).FetchQuota(context.Background(), provider.Credentials{APIKey: "user_ok"})
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if snap.Status != quota.StatusAvailable {
		t.Fatalf("Status = %q, want available", snap.Status)
	}
	if snap.Provider != "cmc" {
		t.Errorf("Provider = %q, want cmc", snap.Provider)
	}
	// The key must never appear as the identity.
	if snap.Identity == "user_ok" {
		t.Error("Identity leaked the API key")
	}

	byName := map[string]quota.Window{}
	for _, w := range snap.Windows {
		byName[w.Name] = w
	}
	monthly, ok := byName["monthly"]
	if !ok {
		t.Fatalf("missing monthly window, got %v", snap.Windows)
	}
	if monthly.WindowMinutes != monthlyMinutes {
		t.Errorf("monthly WindowMinutes = %d, want %d", monthly.WindowMinutes, monthlyMinutes)
	}
	if monthly.UsedPercent != 0 {
		t.Errorf("monthly UsedPercent = %v, want 0 without a configured allowance", monthly.UsedPercent)
	}
	// The upstream reports the period start; the dashboard needs the next
	// boundary, or it renders the past timestamp as "now".
	if want := time.Date(2026, 11, 26, 13, 40, 58, 0, time.UTC); !monthly.ResetAt.Equal(want) {
		t.Errorf("monthly ResetAt = %v, want next boundary %v", monthly.ResetAt, want)
	}
}

func TestCommandCodeFetchQuotaAppliesConfiguredCaps(t *testing.T) {
	ts := usageServer(t, usageBody)
	defer ts.Close()

	p := New(provider.Config{
		Name:    "cmc",
		BaseURL: ts.URL,
		Timeout: 5 * time.Second,
		Quota: provider.QuotaConfig{
			MonthlyCredits: 100,
			FiveHourCap:    3,
			WeeklyCap:      6,
		},
	}, &memoryCredStore{creds: provider.Credentials{APIKey: "user_ok"}})

	snap, err := p.(provider.QuotaFetcher).FetchQuota(context.Background(), provider.Credentials{APIKey: "user_ok"})
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	byName := map[string]quota.Window{}
	for _, w := range snap.Windows {
		byName[w.Name] = w
	}
	monthly := byName["monthly"]
	if monthly.UsedPercent < 0.4 || monthly.UsedPercent > 0.6 {
		t.Fatalf("monthly UsedPercent = %v, want ~0.48 (0.4827/100)", monthly.UsedPercent)
	}
	// 5-hour and weekly spend is not exposed upstream, so they stay unknown
	// rather than being fabricated from the monthly figure.
	for _, name := range []string{"5-hour", "weekly"} {
		w, ok := byName[name]
		if !ok {
			t.Fatalf("missing %s window", name)
		}
		if w.UsedPercent != 0 {
			t.Errorf("%s UsedPercent = %v, want 0 (not measurable upstream)", name, w.UsedPercent)
		}
	}
	if byName["5-hour"].WindowMinutes != fiveHourMinutes {
		t.Errorf("5-hour WindowMinutes = %d", byName["5-hour"].WindowMinutes)
	}
	if byName["weekly"].WindowMinutes != weeklyMinutes {
		t.Errorf("weekly WindowMinutes = %d", byName["weekly"].WindowMinutes)
	}
	// Configured caps surface as the allowance label, since no percentage can.
	if got := byName["5-hour"].Display; got != "cap 3 credits" {
		t.Errorf("5-hour Display = %q, want cap 3 credits", got)
	}
	if got := byName["weekly"].Display; got != "cap 6 credits" {
		t.Errorf("weekly Display = %q, want cap 6 credits", got)
	}
}

func TestCommandCodeFetchQuotaUnavailableNeverDrains(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer ts.Close()

	p := New(provider.Config{Name: "cmc", BaseURL: ts.URL, Timeout: 5 * time.Second},
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
	if snap.Reason == "" {
		t.Error("Reason must name why the snapshot is unavailable")
	}
	if snap.Reason != "" && strings.Contains(snap.Reason, "invalid api key") {
		t.Error("Reason leaked the upstream body")
	}
}

func TestCommandCodeFetchQuotaMissingKey(t *testing.T) {
	p := New(provider.Config{Name: "cmc", Timeout: 5 * time.Second}, &memoryCredStore{})
	snap, err := p.(provider.QuotaFetcher).FetchQuota(context.Background(), provider.Credentials{})
	if err == nil {
		t.Fatal("expected an error with no credentials")
	}
	if snap.Status != quota.StatusUnavailable {
		t.Fatalf("Status = %q, want unavailable", snap.Status)
	}
}

func TestCommandCodeUsageWindowsOrdered(t *testing.T) {
	ts := usageServer(t, usageBody)
	defer ts.Close()
	p := New(provider.Config{Name: "cmc", BaseURL: ts.URL, Timeout: 5 * time.Second},
		&memoryCredStore{creds: provider.Credentials{APIKey: "user_ok"}})
	snap, _ := p.(provider.QuotaFetcher).FetchQuota(context.Background(), provider.Credentials{APIKey: "user_ok"})
	if len(snap.Windows) != 3 {
		t.Fatalf("windows = %d, want 3 (5-hour, weekly, monthly)", len(snap.Windows))
	}
	var raw map[string]any
	encoded, _ := json.Marshal(snap.Raw)
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("Raw is not JSON: %v", err)
	}
}

func TestNextPeriodStartClampsMonthEnd(t *testing.T) {
	cases := []struct{ start, want string }{
		{"2026-10-26T13:40:58.000Z", "2026-11-26T13:40:58Z"},
		{"2026-01-31T00:00:00Z", "2026-02-28T00:00:00Z"}, // February has no 31st
		{"2026-12-31T05:00:00Z", "2027-01-31T05:00:00Z"}, // year rollover
	}
	for _, c := range cases {
		got := nextPeriodStart(c.start)
		want, _ := time.Parse(time.RFC3339, c.want)
		if !got.Equal(want) {
			t.Errorf("nextPeriodStart(%s) = %v, want %v", c.start, got, want)
		}
	}
	if got := nextPeriodStart("not-a-date"); !got.IsZero() {
		t.Errorf("unparseable start = %v, want zero", got)
	}
}

func TestClampPercentRejectsNonFinite(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{math.NaN(), 0},
		{math.Inf(1), 0},
		{math.Inf(-1), 0},
		{-5, 0},
		{150, 100},
		{42.5, 42.5},
	}
	for _, c := range cases {
		if got := clampPercent(c.in); got != c.want {
			t.Errorf("clampPercent(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
