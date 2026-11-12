package commandcode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func quotaTestProvider(t *testing.T, whoami string, status int) (*Provider, *memStore) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/alpha/whoami") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(whoami))
	}))
	t.Cleanup(srv.Close)
	p := New(provider.Config{Name: "cc", BaseURL: srv.URL}, nil).(*Provider)
	store := &memStore{accounts: []provider.Credentials{{APIKey: "k", AccountID: "user-1", Email: "e@example.com", ProjectID: "org-9"}}}
	return p, store
}

func TestFetchQuotaWindowsAndPlan(t *testing.T) {
	whoami, _ := json.Marshal(map[string]any{
		"userId": "user-1",
		"org":    map[string]any{"id": "org-9", "plan": "go", "planId": "go"},
		"credits": map[string]any{
			"remaining": 8.0, "used": 2.0, "limit": 10.0,
		},
		"windowLimits": map[string]any{
			"fiveHour": map[string]any{"used": 1.0, "limit": 5.0, "remaining": 4.0},
			"weekly":   map[string]any{"used": 9.0, "limit": 10.0, "remaining": 1.0},
		},
	})
	p, store := quotaTestProvider(t, string(whoami), 0)
	snap, err := p.FetchQuota(context.Background(), store.accounts[0])
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if snap.Status != "available" {
		t.Fatalf("status = %q reason %q", snap.Status, snap.Reason)
	}
	if snap.PlanType != "go" {
		t.Fatalf("plan = %q", snap.PlanType)
	}
	if snap.Identity != store.accounts[0].Identity() {
		t.Fatalf("identity = %q", snap.Identity)
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	var monthly, session, weekly bool
	for _, w := range snap.Windows {
		switch w.Name {
		case "monthly":
			monthly = true
			if w.UsedPercent < 19 || w.UsedPercent > 21 {
				t.Fatalf("monthly used = %v", w.UsedPercent)
			}
		case "session":
			session = true
			if w.WindowMinutes != 300 {
				t.Fatalf("session minutes = %d", w.WindowMinutes)
			}
		case "weekly":
			weekly = true
			if w.WindowMinutes != 10080 {
				t.Fatalf("weekly minutes = %d", w.WindowMinutes)
			}
		}
	}
	if !monthly || !session || !weekly {
		t.Fatalf("windows = %+v", snap.Windows)
	}
}

func TestFetchQuotaUnauthorized(t *testing.T) {
	p, store := quotaTestProvider(t, "", http.StatusUnauthorized)
	snap, err := p.FetchQuota(context.Background(), store.accounts[0])
	if err == nil {
		t.Fatal("expected error")
	}
	if snap.Status != "unavailable" {
		t.Fatalf("status = %q", snap.Status)
	}
}

func TestFetchQuotaOrgFromWhoami(t *testing.T) {
	whoami := `{"userId":"u","org":{"id":"org-dynamic","plan":"pro"},"credits":{"remaining":5,"limit":10}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(whoami))
	}))
	defer srv.Close()
	p := New(provider.Config{Name: "cc", BaseURL: srv.URL}, nil).(*Provider)
	snap, err := p.FetchQuota(context.Background(), provider.Credentials{APIKey: "k", AccountID: "u"})
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if raw, ok := snap.Raw.(map[string]any); !ok || raw["org"] != "org-dynamic" {
		t.Fatalf("raw = %#v", snap.Raw)
	}
	if snap.PlanType != "pro" {
		t.Fatalf("plan = %q", snap.PlanType)
	}
}

func TestModelsDynamicAndFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"m1","name":"Model One"},{"id":"m2"}]}`))
	}))
	defer srv.Close()
	p := New(provider.Config{Name: "cc", BaseURL: srv.URL}, nil).(*Provider)
	p.modelsURL = srv.URL
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].Name != "Model One" || models[1].Name != "m2" {
		t.Fatalf("models = %#v", models)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	p2 := New(provider.Config{Name: "cc", BaseURL: bad.URL}, nil).(*Provider)
	p2.modelsURL = bad.URL
	models2, err := p2.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models2) != len(DefaultGoModels) {
		t.Fatalf("fallback models = %d, want %d", len(models2), len(DefaultGoModels))
	}

	p3 := New(provider.Config{Name: "cc", Models: []string{"cfg-1"}}, nil).(*Provider)
	p3.modelsURL = bad.URL
	models3, err := p3.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models3) != 1 || models3[0].ID != "cfg-1" {
		t.Fatalf("config fallback = %#v", models3)
	}
}
