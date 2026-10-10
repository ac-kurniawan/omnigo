package codebuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/quota"
)

func TestCodebuddyFetchQuota(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/billing/meter/get-user-resource" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-jwt" {
			t.Errorf("expected Bearer test-jwt, got %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"code": 0,
			"msg": "OK",
			"data": {
				"Response": {
					"Data": {
						"TotalCount": 2,
						"TotalDosage": 5,
						"Accounts": [
							{
								"AccountId": 1001,
								"PackageName": "Bonus Pack",
								"CapacityRemain": 200,
								"CapacitySize": 250,
								"CycleEndTime": "2026-10-22 17:25:37"
							},
							{
								"AccountId": 1002,
								"PackageName": "Free Plan",
								"CapacityRemain": 100,
								"CapacitySize": 100,
								"CycleEndTime": "2026-10-31 23:59:59"
							}
						]
					}
				}
			}
		}`))
	}))
	defer ts.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "test-jwt"}}
	p := New(provider.Config{Name: "codebuddy-intl", BaseURL: ts.URL}, store, true)
	fetcher, ok := p.(provider.QuotaFetcher)
	if !ok {
		t.Fatalf("expected provider to implement provider.QuotaFetcher")
	}

	snap, err := fetcher.FetchQuota(context.Background(), provider.Credentials{APIKey: "test-jwt", Email: "test@example.com"})
	if err != nil {
		t.Fatalf("FetchQuota failed: %v", err)
	}

	if snap.Status != quota.StatusAvailable {
		t.Errorf("expected StatusAvailable, got %v", snap.Status)
	}
	if len(snap.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(snap.Groups))
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("expected 3 windows (Total + 2 accounts), got %d", len(snap.Windows))
	}
	// Total: (200+100) / (250+100) = 300 / 350 = 85.7% remaining -> 14.3% used
	if got := snap.Windows[0].RemainingPercent(); got < 85.0 || got > 86.0 {
		t.Errorf("expected total remaining 85.7%%, got %.1f%%", got)
	}
}
