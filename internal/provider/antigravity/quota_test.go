package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/quota"
)

// antigravityQuotaAccount is a healthy, unexpired credential the fetcher can
// use without triggering a token refresh.
func antigravityQuotaAccount() provider.Credentials {
	return provider.Credentials{
		AccessToken: "quota-token",
		AccountID:   "google-acct-1",
		Email:       "ops@example.com",
		ProjectID:   "proj-42",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
}

// serveQuota starts a fake retrieveUserQuotaSummary upstream and points
// baseURL at it, restoring the real endpoint when the test ends.
func serveQuota(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	orig := baseURL
	baseURL = srv.URL
	t.Cleanup(func() {
		baseURL = orig
		srv.Close()
	})
	return srv
}

// summaryPayload mirrors the live retrieveUserQuotaSummary shape: groups with
// shared weekly and five-hour buckets.
const summaryPayload = `{
	"groups": [
		{"displayName":"Gemini Models","description":"Models within this group: Gemini Flash, Gemini Pro","buckets":[
			{"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","resetTime":"2026-09-29T09:12:52Z","remainingFraction":0.8564},
			{"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","resetTime":"2026-09-22T14:12:52Z","remainingFraction":1}
		]},
		{"displayName":"Claude and GPT models","description":"Models within this group: Claude Opus, Claude Sonnet, GPT-OSS","buckets":[
			{"bucketId":"3p-weekly","displayName":"Weekly Limit Remaining","window":"weekly","resetTime":"2026-09-29T09:12:52Z","remainingFraction":1},
			{"bucketId":"3p-5h","displayName":"Five Hour Limit Remaining","window":"5h","resetTime":"2026-09-22T14:12:52Z","remainingFraction":1}
		]}
	],
	"description":"Within each group, models share a weekly limit and a 5-hour limit."
}`

func TestFetchQuotaGroupsShareWeeklyAndFiveHourWindows(t *testing.T) {
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			t.Errorf("path = %s, want summary endpoint", r.URL.Path)
		}
		_, _ = w.Write([]byte(summaryPayload))
	})

	p := New(provider.Config{Name: "agy"}, staticStore{antigravityQuotaAccount()})
	account := antigravityQuotaAccount()
	snap, err := p.(*Provider).FetchQuota(context.Background(), account)
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if snap.Status != quota.StatusAvailable {
		t.Fatalf("status = %q, want available (reason %q)", snap.Status, snap.Reason)
	}
	if len(snap.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(snap.Groups))
	}
	if snap.Groups[0].Name != "Gemini Models" || len(snap.Groups[0].Windows) != 2 {
		t.Fatalf("group[0] = %+v", snap.Groups[0])
	}
	gemWeekly := snap.Groups[0].Windows[0]
	// Name is the metric-safe bucket id; Display carries the group for labels.
	if gemWeekly.Name != "gemini-weekly" || gemWeekly.Display != "Gemini Models" || gemWeekly.WindowMinutes != 10080 {
		t.Fatalf("gemini weekly window = %+v", gemWeekly)
	}
	// 0.8564 remaining => 14.36 used => 85.64 remaining displayed
	if got := gemWeekly.RemainingPercent(); got != 85.64 {
		t.Fatalf("weekly remaining = %v, want 85.64", got)
	}
	if snap.Groups[1].Name != "Claude and GPT models" {
		t.Fatalf("group[1] = %+v", snap.Groups[1])
	}
	if len(snap.Windows) != 4 {
		t.Fatalf("flattened windows = %d, want 4", len(snap.Windows))
	}
	// Flattened windows must carry the machine id and the group display name.
	found := map[string]bool{}
	for _, w := range snap.Windows {
		found[w.Name+"/"+w.Label()] = true
	}
	for _, want := range []string{
		"gemini-weekly/Gemini Models · Weekly limit",
		"gemini-5h/Gemini Models · 5-hour limit",
		"3p-weekly/Claude and GPT models · Weekly limit",
		"3p-5h/Claude and GPT models · 5-hour limit",
	} {
		if !found[want] {
			t.Fatalf("missing flattened window %q", want)
		}
	}
	if snap.Raw == nil {
		t.Fatal("Raw payload not retained")
	}
}

func TestFetchQuotaWeeklyExhaustedMarksGroupExhausted(t *testing.T) {
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"groups": [
				{"displayName":"Gemini Models","buckets":[
					{"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","resetTime":"2026-09-29T09:12:52Z","remainingFraction":1},
					{"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","resetTime":"2026-09-22T14:12:52Z","remainingFraction":1}
				]},
				{"displayName":"Claude and GPT models","buckets":[
					{"bucketId":"3p-weekly","displayName":"Weekly Limit Remaining","window":"weekly","resetTime":"2026-09-29T09:12:52Z","remainingFraction":0},
					{"bucketId":"3p-5h","displayName":"Five Hour Limit Remaining","window":"5h","resetTime":"2026-09-22T14:12:52Z","remainingFraction":1}
				]}
			]
		}`))
	})

	p := New(provider.Config{Name: "agy"}, staticStore{antigravityQuotaAccount()})
	snap, err := p.(*Provider).FetchQuota(context.Background(), antigravityQuotaAccount())
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if snap.Status != quota.StatusExhausted {
		t.Fatalf("status = %q, want exhausted", snap.Status)
	}
	if !strings.Contains(snap.Reason, "Claude and GPT models") || !strings.Contains(snap.Reason, "Weekly") {
		t.Fatalf("reason = %q, want exhausted group + window", snap.Reason)
	}
	// Zero weekly bucket must carry the reset so the UI can count down.
	var weekly *quota.Window
	for i := range snap.Windows {
		if snap.Windows[i].WindowMinutes == 10080 && snap.Windows[i].Name == "3p-weekly" {
			weekly = &snap.Windows[i]
		}
	}
	if weekly == nil || weekly.UsedPercent != 100 {
		t.Fatalf("exhausted weekly window = %+v", weekly)
	}
	if want := time.Date(2026, 9, 29, 9, 12, 52, 0, time.UTC); !weekly.ResetAt.Equal(want) {
		t.Fatalf("reset = %v, want %v", weekly.ResetAt, want)
	}
}

func TestFetchQuotaZeroBucketWithoutResetIsUnavailable(t *testing.T) {
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"groups": [{"displayName":"Gemini Models","buckets":[
				{"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","remainingFraction":0},
				{"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","remainingFraction":1}
			]}]
		}`))
	})

	p := New(provider.Config{Name: "agy"}, staticStore{antigravityQuotaAccount()})
	snap, err := p.(*Provider).FetchQuota(context.Background(), antigravityQuotaAccount())
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if snap.Status != quota.StatusUnavailable {
		t.Fatalf("status = %q, want unavailable", snap.Status)
	}
	if snap.Reason == "" {
		t.Fatal("unavailable snapshot missing reason")
	}
}

func TestFetchQuotaUnauthorizedIsUnavailable(t *testing.T) {
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	})

	p := New(provider.Config{Name: "agy"}, staticStore{antigravityQuotaAccount()})
	snap, err := p.(*Provider).FetchQuota(context.Background(), antigravityQuotaAccount())
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if snap.Status != quota.StatusUnavailable {
		t.Fatalf("status = %q, want unavailable", snap.Status)
	}
	if !strings.Contains(snap.Reason, "401") {
		t.Fatalf("reason = %q, want upstream status 401", snap.Reason)
	}
	if strings.Contains(err.Error(), "quota-token") || strings.Contains(snap.Reason, "quota-token") {
		t.Fatal("credential leaked into error or reason")
	}
}

func TestFetchQuotaEmptyGroupsIsUnavailable(t *testing.T) {
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"groups":[]}`))
	})

	p := New(provider.Config{Name: "agy"}, staticStore{antigravityQuotaAccount()})
	snap, err := p.(*Provider).FetchQuota(context.Background(), antigravityQuotaAccount())
	if err == nil {
		t.Fatal("expected error for empty groups")
	}
	if snap.Status != quota.StatusUnavailable {
		t.Fatalf("status = %q, want unavailable", snap.Status)
	}
	if !strings.Contains(snap.Reason, "no quota groups") {
		t.Fatalf("reason = %q, want no quota groups", snap.Reason)
	}
}

func TestFetchQuotaSendsAuthAndProject(t *testing.T) {
	var gotAuth, gotUA, gotGoog, gotCT, gotBody string
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotGoog = r.Header.Get("X-Goog-Api-Client")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","remainingFraction":1}]}]}`))
	})

	p := New(provider.Config{Name: "agy"}, staticStore{antigravityQuotaAccount()})
	if _, err := p.(*Provider).FetchQuota(context.Background(), antigravityQuotaAccount()); err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if gotAuth != "Bearer quota-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotUA != antigravityUserAgent || gotGoog != antigravityGoogAPI {
		t.Fatalf("headers: UA=%q Goog=%q", gotUA, gotGoog)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type = %q", gotCT)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body %q: %v", gotBody, err)
	}
	if body["project"] != "proj-42" {
		t.Fatalf("body project = %q, want proj-42", body["project"])
	}
}

func TestFetchQuotaOmitsEmptyProject(t *testing.T) {
	var gotBody string
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","remainingFraction":1}]}]}`))
	})

	account := antigravityQuotaAccount()
	account.ProjectID = ""
	p := New(provider.Config{Name: "agy"}, staticStore{account})
	if _, err := p.(*Provider).FetchQuota(context.Background(), account); err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if strings.Contains(gotBody, "project") {
		t.Fatalf("body = %q, want no project key", gotBody)
	}
}

func TestProviderImplementsQuotaFetcher(t *testing.T) {
	p := New(provider.Config{Name: "agy"}, staticStore{antigravityQuotaAccount()})
	if _, ok := p.(provider.QuotaFetcher); !ok {
		t.Fatal("*Provider does not implement provider.QuotaFetcher")
	}
}

func TestFetchQuotaMissingRemainingFractionIsUnavailable(t *testing.T) {
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"groups": [{"displayName":"Gemini Models","buckets":[
				{"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h"}
			]}]
		}`))
	})

	p := New(provider.Config{Name: "agy"}, staticStore{antigravityQuotaAccount()})
	snap, err := p.(*Provider).FetchQuota(context.Background(), antigravityQuotaAccount())
	if err != nil {
		t.Fatalf("FetchQuota returned unexpected error: %v", err)
	}
	if snap.Status != quota.StatusUnavailable {
		t.Fatalf("status = %q, want unavailable", snap.Status)
	}
	if !strings.Contains(snap.Reason, "no remaining fraction") {
		t.Fatalf("reason = %q, want no remaining fraction", snap.Reason)
	}
}
