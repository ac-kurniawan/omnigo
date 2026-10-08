package muse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestDeviceCode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("x-api-version") != "1.0.0" {
			t.Fatalf("expected x-api-version 1.0.0, got %s", r.Header.Get("x-api-version"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != ClientID {
			t.Fatalf("expected client_id %s, got %s", ClientID, r.Form.Get("client_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dev-123",
			"user_code":                 "USER-456",
			"verification_uri":          "https://auth.meta.com/device",
			"expires_in":                300,
			"interval":                  5,
		})
	}))
	defer ts.Close()

	client := NewOAuthClient(WithDeviceAuthURL(ts.URL))
	resp, err := client.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("RequestDeviceCode failed: %v", err)
	}
	if resp.DeviceCode != "dev-123" || resp.UserCode != "USER-456" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestPollDeviceToken(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        map[string]any
		wantPending bool
		wantToken   string
		wantErr     bool
	}{
		{
			name:        "authorization pending",
			status:      http.StatusBadRequest,
			body:        map[string]any{"error": "authorization_pending"},
			wantPending: true,
		},
		{
			name:        "slow down",
			status:      http.StatusBadRequest,
			body:        map[string]any{"error": "slow_down"},
			wantPending: true,
		},
		{
			name:        "authorized token returned",
			status:      http.StatusOK,
			body:        map[string]any{"access_token": "meta-tok-xyz", "token_type": "Bearer"},
			wantToken:   "meta-tok-xyz",
		},
		{
			name:        "expired token",
			status:      http.StatusBadRequest,
			body:        map[string]any{"error": "expired_token", "error_description": "code expired"},
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("x-api-version") != "1.0.0" {
					t.Errorf("missing x-api-version")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				json.NewEncoder(w).Encode(tt.body)
			}))
			defer ts.Close()

			client := NewOAuthClient(WithTokenURL(ts.URL))
			res, err := client.PollToken(context.Background(), "dev-123")
			if (err != nil) != tt.wantErr {
				t.Fatalf("PollToken err=%v, wantErr=%v", err, tt.wantErr)
			}
			if res.Pending != tt.wantPending {
				t.Errorf("Pending=%v, want %v", res.Pending, tt.wantPending)
			}
			if res.AccessToken != tt.wantToken {
				t.Errorf("AccessToken=%s, want %s", res.AccessToken, tt.wantToken)
			}
		})
	}
}

func TestMintSubscriptionKey(t *testing.T) {
	attempts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if r.Header.Get("Authorization") != "Bearer meta-tok-xyz" {
			t.Fatalf("expected bearer token, got %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("x-api-version") != "1.0.0" {
			t.Fatalf("expected x-api-version 1.0.0")
		}
		if attempts == 1 {
			// First attempt returns 429 to test retry
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate_limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"api_key":         "LLM|minted-key-123",
			"user_email":      "user@example.com",
			"subs_tier_name":  "Muse Code Pro",
			"is_subs_active":  true,
		})
	}))
	defer ts.Close()

	client := NewOAuthClient(
		WithKeyMintURL(ts.URL),
		WithRetryBackoff(1*time.Millisecond), // Fast backoff in tests
	)
	keyResp, err := client.MintSubscriptionKey(context.Background(), "meta-tok-xyz")
	if err != nil {
		t.Fatalf("MintSubscriptionKey failed: %v", err)
	}
	if keyResp.APIKey != "LLM|minted-key-123" {
		t.Errorf("APIKey=%s, want LLM|minted-key-123", keyResp.APIKey)
	}
	if keyResp.UserEmail != "user@example.com" {
		t.Errorf("UserEmail=%s, want user@example.com", keyResp.UserEmail)
	}
	if attempts < 2 {
		t.Errorf("expected retry on 429, total attempts=%d", attempts)
	}
}

func TestMintSubscriptionKeyInactiveSubscription(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"is_subs_active": false,
		})
	}))
	defer ts.Close()

	client := NewOAuthClient(WithKeyMintURL(ts.URL))
	_, err := client.MintSubscriptionKey(context.Background(), "meta-tok-xyz")
	if err == nil {
		t.Fatal("expected error for inactive subscription")
	}
}
