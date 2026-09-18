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

// serveQuota starts a fake retrieveUserQuota upstream and points baseURL at it,
// restoring the real endpoint when the test ends.
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

func TestFetchQuotaAllBucketsAvailable(t *testing.T) {
	// Trimmed shape of the live 27-bucket payload: tokenType WTUS, singular
	// modelId, remainingFraction, and a resetTime only on windowed buckets.
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:retrieveUserQuota" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"buckets": [
				{"tokenType":"WTUS","modelId":"chat_20706","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"gemini-3.8-flash-tiered","remainingFraction":0.5,"resetTime":"2026-09-18T07:17:28Z"},
				{"tokenType":"WTUS","modelId":"claude-sonnet-4-6","remainingFraction":1,"resetTime":"2026-09-21T07:17:28Z"}
			]
		}`))
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
	if snap.Provider != "agy" || snap.Identity != account.Identity() || snap.AccountID != account.AccountID || snap.Email != account.Email {
		t.Fatalf("identity fields = %+v", snap)
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(snap.Windows))
	}
	if snap.Windows[0].Name != "chat_20706" || snap.Windows[0].UsedPercent != 0 {
		t.Fatalf("window[0] = %+v", snap.Windows[0])
	}
	if snap.Windows[1].UsedPercent != 50 {
		t.Fatalf("window[1] used = %v, want 50", snap.Windows[1].UsedPercent)
	}
	wantReset := time.Date(2026, 9, 18, 7, 17, 28, 0, time.UTC)
	if !snap.Windows[1].ResetAt.Equal(wantReset) {
		t.Fatalf("window[1] reset = %v, want %v", snap.Windows[1].ResetAt, wantReset)
	}
	if !snap.Windows[0].ResetAt.IsZero() {
		t.Fatalf("window[0] reset = %v, want zero", snap.Windows[0].ResetAt)
	}
	if snap.ObservedAt.IsZero() {
		t.Fatal("ObservedAt is zero")
	}
	if snap.Raw == nil {
		t.Fatal("Raw payload not retained")
	}
}

func TestFetchQuotaExhaustedWhenZeroBucketHasReset(t *testing.T) {
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"buckets": [
				{"tokenType":"WTUS","modelId":"chat_20706","remainingFraction":1},
				{"resetTime":"2026-09-21T07:17:28Z","tokenType":"WTUS","modelId":"claude-opus-4-6-thinking","remainingFraction":0},
				{"resetTime":"2026-09-18T07:17:28Z","tokenType":"WTUS","modelId":"gemini-3.8-flash-tiered","remainingFraction":1}
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
	if snap.Reason != "claude-opus-4-6-thinking" {
		t.Fatalf("reason = %q, want binding model id", snap.Reason)
	}
	var zero *quota.Window
	for i := range snap.Windows {
		if snap.Windows[i].Name == "claude-opus-4-6-thinking" {
			zero = &snap.Windows[i]
		}
	}
	if zero == nil || zero.UsedPercent != 100 {
		t.Fatalf("exhausted window = %+v", zero)
	}
	if want := time.Date(2026, 9, 21, 7, 17, 28, 0, time.UTC); !zero.ResetAt.Equal(want) {
		t.Fatalf("reset = %v, want %v", zero.ResetAt, want)
	}
}

func TestFetchQuotaZeroBucketWithoutResetIsUnavailable(t *testing.T) {
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"buckets": [
				{"tokenType":"WTUS","modelId":"chat_20706","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"claude-opus-4-6-thinking","remainingFraction":0}
			]
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

func TestFetchQuotaEmptyBucketsIsUnavailable(t *testing.T) {
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"buckets":[]}`))
	})

	p := New(provider.Config{Name: "agy"}, staticStore{antigravityQuotaAccount()})
	snap, err := p.(*Provider).FetchQuota(context.Background(), antigravityQuotaAccount())
	if err == nil {
		t.Fatal("expected error for empty buckets")
	}
	if snap.Status != quota.StatusUnavailable {
		t.Fatalf("status = %q, want unavailable", snap.Status)
	}
	if !strings.Contains(snap.Reason, "no quota buckets") {
		t.Fatalf("reason = %q, want no quota buckets", snap.Reason)
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
		_, _ = w.Write([]byte(`{"buckets":[{"tokenType":"WTUS","modelId":"chat_20706","remainingFraction":1}]}`))
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
		_, _ = w.Write([]byte(`{"buckets":[{"tokenType":"WTUS","modelId":"chat_20706","remainingFraction":1}]}`))
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

func TestFetchQuotaReal27BucketPayloadShape(t *testing.T) {
	// 27-bucket fixture mirroring upstream shape: 24 active, 3 exhausted with reset targets.
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"buckets": [
				{"tokenType":"WTUS","modelId":"chat_20706","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"gemini-1.5-pro","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"gemini-1.5-flash","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"gemini-2.0-flash","remainingFraction":0.9,"resetTime":"2026-09-18T12:00:00Z"},
				{"tokenType":"WTUS","modelId":"gemini-2.0-pro","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"gemini-3.7-flash","remainingFraction":0.75,"resetTime":"2026-09-18T10:00:00Z"},
				{"tokenType":"WTUS","modelId":"gemini-3.7-pro","remainingFraction":0.5,"resetTime":"2026-09-18T08:00:00Z"},
				{"tokenType":"WTUS","modelId":"gemini-3.8-flash-tiered","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"claude-3-5-sonnet","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"claude-3-5-haiku","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"claude-3-opus","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"claude-opus-4-6-thinking","remainingFraction":0,"resetTime":"2026-09-21T07:17:28Z"},
				{"tokenType":"WTUS","modelId":"claude-sonnet-4-6","remainingFraction":0,"resetTime":"2026-09-21T07:17:28Z"},
				{"tokenType":"WTUS","modelId":"gpt-oss-120b-medium","remainingFraction":0,"resetTime":"2026-09-21T07:17:28Z"},
				{"tokenType":"WTUS","modelId":"gpt-4o","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"gpt-4o-mini","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"code-bison","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"code-gecko","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"text-bison","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"chat-bison","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"tab-completion-default","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"tab-completion-smart","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"agent-gemini-pro","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"agent-claude-sonnet","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"reviewer-model","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"embed-gecko","remainingFraction":1},
				{"tokenType":"WTUS","modelId":"embed-multilingual","remainingFraction":1}
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
	if len(snap.Windows) != 27 {
		t.Fatalf("windows count = %d, want 27", len(snap.Windows))
	}
	if snap.Reason != "claude-opus-4-6-thinking" {
		t.Fatalf("reason = %q, want first exhausted model id", snap.Reason)
	}
}

func TestFetchQuotaMissingRemainingFractionIsUnavailable(t *testing.T) {
	serveQuota(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"buckets": [
				{"tokenType":"WTUS","modelId":"chat_20706"}
			]
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
