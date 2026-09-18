package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/quota"
)

// The syncer wires these interfaces; assert the provider still satisfies them.
var (
	_ provider.QuotaFetcher      = (*Provider)(nil)
	_ provider.QuotaHeaderSource = (*Provider)(nil)
)

// liveUsagePayload is the wire JSON captured on 2026-09-18 from
// GET https://chatgpt.com/backend-api/wham/usage. The secondary window is a
// real observed case: used_percent == 100 while the account still serves.
const liveUsagePayload = `{
  "user_id": "user-123",
  "account_id": "fb88ebc3-0000-0000-0000-000000000000",
  "email": "user@domain.com",
  "plan_type": "k12",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window": {"used_percent": 7, "limit_window_seconds": 18000, "reset_after_seconds": 14134, "reset_at": 1789711562},
    "secondary_window": {"used_percent": 100, "limit_window_seconds": 604800, "reset_after_seconds": 40753, "reset_at": 1789738181}
  },
  "model_usage": {"gpt-6-astra": {"available": true, "available_at": null, "credits_would_enable": false}},
  "credits": {"has_credits": false, "unlimited": false, "balance": "0"},
  "spend_control": {"reached": false},
  "rate_limit_reached_type": null
}`

func codexUsageProvider(t *testing.T, server *httptest.Server, creds provider.Credentials) *Provider {
	t.Helper()
	store := &memoryCredStore{creds: creds}
	return New(provider.Config{Name: "codex-main", BaseURL: server.URL, Timeout: time.Second}, store).(*Provider)
}

func freshAccount() provider.Credentials {
	return provider.Credentials{
		AccessToken: "access-token",
		AccountID:   "account-1",
		UserID:      "user-1",
		Email:       "caller@example.com",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
}

func TestUsageURLDerivation(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{name: "default base uses wham", want: "https://chatgpt.com/backend-api/wham/usage"},
		{name: "backend-api base uses wham", base: "https://chatgpt.com/backend-api", want: "https://chatgpt.com/backend-api/wham/usage"},
		{name: "backend-api base with trailing slash", base: "https://chatgpt.com/backend-api/", want: "https://chatgpt.com/backend-api/wham/usage"},
		{name: "non backend-api base uses api codex", base: "https://api.openai.com", want: "https://api.openai.com/api/codex/usage"},
		{name: "proxy base uses api codex", base: "https://proxy.internal/", want: "https://proxy.internal/api/codex/usage"},
		{name: "configured inference endpoint uses wham", base: "https://chatgpt.com/backend-api/codex/responses", want: "https://chatgpt.com/backend-api/wham/usage"},
		{name: "configured responses endpoint uses wham", base: "https://proxy.internal/backend-api/responses", want: "https://proxy.internal/backend-api/wham/usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &memoryCredStore{creds: freshAccount()}
			p := New(provider.Config{Name: "codex-main", BaseURL: tt.base}, store).(*Provider)
			if p.quotaURL != tt.want {
				t.Fatalf("quotaURL = %q, want %q", p.quotaURL, tt.want)
			}
		})
	}
}

func TestFetchQuotaSendsVerifiedHeadersAndMapsLivePayload(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		wantHeaders := map[string]string{
			"Authorization":      "Bearer access-token",
			"chatgpt-account-id": "account-1",
			"Version":            ClientVersion,
			"originator":         Originator,
			"User-Agent":         UserAgent,
			"OpenAI-Beta":        BetaVersion,
			"Accept":             "application/json",
		}
		for key, want := range wantHeaders {
			if got := r.Header.Get(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		_, _ = w.Write([]byte(liveUsagePayload))
	}))
	defer server.Close()

	account := freshAccount()
	p := codexUsageProvider(t, server, account)

	snapshot, err := p.FetchQuota(context.Background(), account)
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if gotPath != "/api/codex/usage" {
		t.Fatalf("path = %q, want /api/codex/usage", gotPath)
	}
	if snapshot.Provider != "codex-main" || snapshot.Identity != account.Identity() {
		t.Fatalf("provider/identity = %q / %q", snapshot.Provider, snapshot.Identity)
	}
	if snapshot.AccountID != "fb88ebc3-0000-0000-0000-000000000000" || snapshot.Email != "user@domain.com" || snapshot.PlanType != "k12" {
		t.Fatalf("identity fields = %+v", snapshot)
	}
	if snapshot.Status != quota.StatusAvailable {
		t.Fatalf("status = %q, want available", snapshot.Status)
	}
	if snapshot.ObservedAt.IsZero() {
		t.Fatal("ObservedAt not set")
	}
	if snapshot.Raw == nil {
		t.Fatal("Raw payload not retained")
	}
	if len(snapshot.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(snapshot.Windows))
	}
	primary, secondary := snapshot.Windows[0], snapshot.Windows[1]
	if primary.Name != "primary" || primary.UsedPercent != 7 || primary.WindowMinutes != 300 || primary.ResetAfter != 14134*time.Second {
		t.Fatalf("primary = %+v", primary)
	}
	if want := time.Unix(1789711562, 0).UTC(); !primary.ResetAt.Equal(want) {
		t.Fatalf("primary reset = %v, want %v", primary.ResetAt, want)
	}
	if secondary.Name != "secondary" || secondary.UsedPercent != 100 || secondary.WindowMinutes != 10080 {
		t.Fatalf("secondary = %+v", secondary)
	}
}

func TestFetchQuotaUsesWhamPathForBackendAPIBase(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(liveUsagePayload))
	}))
	defer server.Close()

	account := freshAccount()
	store := &memoryCredStore{creds: account}
	p := New(provider.Config{Name: "codex-main", BaseURL: server.URL + "/backend-api"}, store).(*Provider)
	if _, err := p.FetchQuota(context.Background(), account); err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if gotPath != "/backend-api/wham/usage" {
		t.Fatalf("path = %q, want /backend-api/wham/usage", gotPath)
	}
}

// Live-verified regression: a secondary window at 100% used does NOT mean the
// account is exhausted. Upstream keeps serving while allowed stays true.
func TestFetchQuotaSecondaryFullButAllowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(liveUsagePayload))
	}))
	defer server.Close()

	account := freshAccount()
	p := codexUsageProvider(t, server, account)
	snapshot, err := p.FetchQuota(context.Background(), account)
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if snapshot.Windows[1].UsedPercent != 100 {
		t.Fatalf("secondary used_percent = %v, want 100", snapshot.Windows[1].UsedPercent)
	}
	if snapshot.Status != quota.StatusAvailable {
		t.Fatalf("status = %q, want available when allowed == true", snapshot.Status)
	}
}

func TestFetchQuotaExhaustedOnlyOnExplicitSignal(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    quota.Status
	}{
		{
			name:    "allowed false",
			payload: `{"rate_limit":{"allowed":false,"limit_reached":false,"secondary_window":{"used_percent":100}}}`,
			want:    quota.StatusExhausted,
		},
		{
			name:    "limit reached true with allowed true",
			payload: `{"rate_limit":{"allowed":true,"limit_reached":true,"primary_window":{"used_percent":12}}}`,
			want:    quota.StatusExhausted,
		},
		{
			name:    "secondary full but allowed",
			payload: `{"rate_limit":{"allowed":true,"limit_reached":false,"secondary_window":{"used_percent":100}}}`,
			want:    quota.StatusAvailable,
		},
		{
			name:    "missing rate limit is unavailable",
			payload: `{"email":"user@domain.com"}`,
			want:    quota.StatusUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tt.payload))
			}))
			defer server.Close()
			account := freshAccount()
			p := codexUsageProvider(t, server, account)
			snapshot, err := p.FetchQuota(context.Background(), account)
			if snapshot.Status != tt.want {
				t.Fatalf("status = %q, want %q (err = %v)", snapshot.Status, tt.want, err)
			}
			if tt.want == quota.StatusUnavailable && err == nil {
				t.Fatal("unavailable snapshot returned nil error")
			}
		})
	}
}

func TestFetchQuotaFailureStatusesAreUnavailableAndSanitized(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		reason string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"secret-upstream-token"}`, reason: "upstream status 401"},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":"secret-upstream-token"}`, reason: "upstream status 403"},
		{name: "server error", status: http.StatusInternalServerError, body: `{"error":"secret-upstream-token"}`, reason: "upstream status 500"},
		{name: "malformed body", status: http.StatusOK, body: `{not json`, reason: "invalid quota response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			account := freshAccount()
			p := codexUsageProvider(t, server, account)
			snapshot, err := p.FetchQuota(context.Background(), account)
			if err == nil {
				t.Fatal("expected error")
			}
			if snapshot.Status != quota.StatusUnavailable {
				t.Fatalf("status = %q, want unavailable", snapshot.Status)
			}
			if snapshot.Reason != tt.reason {
				t.Fatalf("reason = %q, want %q", snapshot.Reason, tt.reason)
			}
			if strings.Contains(snapshot.Reason, "secret-upstream-token") || strings.Contains(err.Error(), "secret-upstream-token") {
				t.Fatalf("credential leaked: reason=%q err=%v", snapshot.Reason, err)
			}
			if snapshot.Provider != "codex-main" || snapshot.Identity != account.Identity() {
				t.Fatalf("identity dropped on failure: %+v", snapshot)
			}
		})
	}
}

func TestFetchQuotaRefreshesExpiredTokenFirst(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })

	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-new",
			"refresh_token": "refresh-new",
			"expires_in":    3600,
		})
	}))
	defer auth.Close()
	tokenURL = auth.URL

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(liveUsagePayload))
	}))
	defer server.Close()

	account := provider.Credentials{
		AccessToken:  "access-old",
		RefreshToken: "refresh-old",
		AccountID:    "account-1",
		ExpiresAt:    time.Now().Add(time.Minute),
	}
	p := codexUsageProvider(t, server, account)
	if _, err := p.FetchQuota(context.Background(), account); err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if gotAuth != "Bearer access-new" {
		t.Fatalf("authorization = %q, want refreshed token", gotAuth)
	}
}

func TestQuotaFromHeadersParsesLiveInferenceHeaders(t *testing.T) {
	header := http.Header{}
	header.Set("x-codex-primary-used-percent", "7")
	header.Set("x-codex-primary-window-minutes", "300")
	header.Set("x-codex-primary-reset-at", "1789711562")
	header.Set("x-codex-secondary-used-percent", "100")
	header.Set("x-codex-secondary-window-minutes", "10080")
	header.Set("x-codex-secondary-reset-at", "1789738181")

	account := freshAccount()
	store := &memoryCredStore{creds: account}
	p := New(provider.Config{Name: "codex-main"}, store).(*Provider)
	snapshot, ok := p.QuotaFromHeaders(account, header)
	if !ok {
		t.Fatal("headers not recognized")
	}
	if snapshot.Status != quota.StatusAvailable {
		t.Fatalf("status = %q, want available", snapshot.Status)
	}
	if snapshot.Identity != account.Identity() || snapshot.Provider != "codex-main" {
		t.Fatalf("identity fields = %+v", snapshot)
	}
	if len(snapshot.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(snapshot.Windows))
	}
	primary := snapshot.Windows[0]
	if primary.Name != "primary" || primary.UsedPercent != 7 || primary.WindowMinutes != 300 {
		t.Fatalf("primary = %+v", primary)
	}
	if want := time.Unix(1789711562, 0).UTC(); !primary.ResetAt.Equal(want) {
		t.Fatalf("primary reset = %v, want %v", primary.ResetAt, want)
	}
	if snapshot.Windows[1].UsedPercent != 100 {
		t.Fatalf("secondary = %+v", snapshot.Windows[1])
	}
}

func TestQuotaFromHeadersAbsentReturnsFalse(t *testing.T) {
	account := freshAccount()
	store := &memoryCredStore{creds: account}
	p := New(provider.Config{Name: "codex-main"}, store).(*Provider)
	if snapshot, ok := p.QuotaFromHeaders(account, http.Header{}); ok {
		t.Fatalf("ok = true for empty headers: %+v", snapshot)
	}
}

func TestChatCompletionFeedsQuotaObserver(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-codex-primary-used-percent", "7")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		w.Header().Set("x-codex-primary-reset-at", "1789711562")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexTestStream()))
	}))
	defer server.Close()

	account := freshAccount()
	store := &memoryCredStore{creds: account}
	p := New(provider.Config{Name: "codex-main", BaseURL: server.URL, Timeout: time.Second}, store).(*Provider)

	observed := make(chan quota.AccountSnapshot, 1)
	p.SetQuotaObserver(func(snapshot quota.AccountSnapshot) { observed <- snapshot })

	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gpt-5.3-codex", Stream: true, Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, httptest.NewRecorder()); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	select {
	case snapshot := <-observed:
		if snapshot.Identity != account.Identity() || snapshot.Status != quota.StatusAvailable {
			t.Fatalf("snapshot = %+v", snapshot)
		}
		if len(snapshot.Windows) != 1 || snapshot.Windows[0].UsedPercent != 7 {
			t.Fatalf("windows = %+v", snapshot.Windows)
		}
	case <-time.After(time.Second):
		t.Fatal("observer not notified from inference headers")
	}

	if captured, ok := p.CapturedQuota(account); !ok || captured.Windows[0].UsedPercent != 7 {
		t.Fatalf("CapturedQuota = %+v, ok = %v", captured, ok)
	}
}

func TestChatCompletionWithoutQuotaHeadersSkipsObserver(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexTestStream()))
	}))
	defer server.Close()

	account := freshAccount()
	store := &memoryCredStore{creds: account}
	p := New(provider.Config{Name: "codex-main", BaseURL: server.URL, Timeout: time.Second}, store).(*Provider)

	called := false
	p.SetQuotaObserver(func(quota.AccountSnapshot) { called = true })
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gpt-5.3-codex", Stream: true, Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, httptest.NewRecorder()); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if called {
		t.Fatal("observer called without quota headers")
	}
}
