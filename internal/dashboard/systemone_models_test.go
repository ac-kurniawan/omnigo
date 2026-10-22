package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
)

// A dashboard request for the available System One models returns the models
// the upstream account can use, so an operator can pick one without leaving
// the dashboard.
func TestSystemOneModelsEndpointListsModels(t *testing.T) {
	// The endpoint reads the key from the environment, so the test sets it
	// explicitly rather than inheriting whatever the runner has.
	t.Setenv("TYPESAFE_API_KEY", "test-key")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("upstream path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"jev-latest","description":"flagship"},{"name":"jev-preview","description":"preview"}]}`))
	}))
	t.Cleanup(upstream.Close)

	prev := systemOneModelsBaseURL
	systemOneModelsBaseURL = upstream.URL
	t.Cleanup(func() { systemOneModelsBaseURL = prev })

	srv := newSmartDashboard(t, &config.Config{})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/system-one/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Models []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(payload.Models) != 2 {
		t.Fatalf("models = %d, want 2 (%+v)", len(payload.Models), payload.Models)
	}
	if payload.Models[0].Name != "jev-latest" {
		t.Errorf("models[0].name = %q, want jev-latest", payload.Models[0].Name)
	}
	if payload.Models[1].Description != "preview" {
		t.Errorf("models[1].description = %q, want preview", payload.Models[1].Description)
	}
}

// Without an API key the endpoint reports the problem instead of pretending
// the account has no models.
func TestSystemOneModelsEndpointReportsMissingKey(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")

	srv := newSmartDashboard(t, &config.Config{})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/system-one/models", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %q)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "TYPESAFE_API_KEY") {
		t.Errorf("body = %q, want it to name TYPESAFE_API_KEY", body)
	}
}
