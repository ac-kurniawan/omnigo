package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestInternalEndpointsRejectInvalidDashboardAuthorization(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "missing headers", mutate: func(r *http.Request) {
			r.Header.Del("HX-Request")
			r.Header.Del("Origin")
			r.Header.Del("X-Omnigo-CSRF")
			r.Header.Del("Cookie")
		}},
		{name: "missing token", mutate: func(r *http.Request) { r.Header.Del("X-Omnigo-CSRF") }},
		{name: "invalid token", mutate: func(r *http.Request) { r.Header.Set("X-Omnigo-CSRF", "wrong") }},
		{name: "cross origin", mutate: func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }},
		{name: "cross origin referer", mutate: func(r *http.Request) {
			r.Header.Del("Origin")
			r.Header.Set("Referer", "https://evil.example/dashboard")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := dashboardRequest("/internal/test/openai")
			tt.mutate(req)
			rr := httptest.NewRecorder()
			testRouter(t, &config.Config{}, &vault.Vault{}).ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rr.Code)
			}
			if !strings.Contains(rr.Body.String(), `"code":"dashboard_authorization_failed"`) {
				t.Fatalf("body = %s", rr.Body.String())
			}
		})
	}
}

func TestInternalEndpointRejectsMismatchedOriginEvenWithMatchingReferer(t *testing.T) {
	req := dashboardRequest("/internal/test/openai")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Referer", "http://omnigo.test/providers")
	rr := httptest.NewRecorder()
	testRouter(t, &config.Config{}, &vault.Vault{}).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestInternalEndpointAcceptsMatchingReferer(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name}
	})
	cfg := &config.Config{Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x"}}}
	req := dashboardRequest("/internal/test/openai")
	req.Header.Del("Origin")
	req.Header.Set("Referer", "http://omnigo.test/providers")
	rr := httptest.NewRecorder()
	testRouter(t, cfg, &vault.Vault{}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
}

func TestInternalRefreshModels(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, models: []provider.Model{{ID: "gpt-4o"}, {ID: "gpt-4o-mini"}}}
	})
	cfg := &config.Config{Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x"}}}
	req := dashboardRequest("/internal/refresh-models/openai")
	rr := httptest.NewRecorder()
	testRouter(t, cfg, &vault.Vault{}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "gpt-4o") {
		t.Fatalf("body = %s", rr.Body.String())
	}
	// persistence: the provider's cached model list is updated in-memory
	if len(cfg.Providers[0].Models) != 2 || cfg.Providers[0].Models[0] != "gpt-4o" {
		t.Fatalf("models not persisted: %+v", cfg.Providers[0].Models)
	}
}

func dashboardRequest(path string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Host = "omnigo.test"
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Origin", "http://omnigo.test")
	req.Header.Set("X-Omnigo-CSRF", "test-dashboard-token")
	req.AddCookie(&http.Cookie{Name: "omnigo_csrf", Value: "test-dashboard-token"})
	return req
}

func TestInternalTestProvider(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name}
	})
	cfg := &config.Config{Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x"}}}
	req := dashboardRequest("/internal/test/openai")
	rr := httptest.NewRecorder()
	testRouter(t, cfg, &vault.Vault{}).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestInternalEndpointsDisabledWithDashboard(t *testing.T) {
	off := false
	cfg := &config.Config{Dashboard: config.Dashboard{Enabled: &off}}
	for _, path := range []string{"/internal/test/openai", "/internal/refresh-models/openai"} {
		// A deliberately invalid CSRF/origin request: while disabled the path
		// must be indistinguishable from unregistered, so 404 wins over 403.
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rr := httptest.NewRecorder()
		testRouter(t, cfg, &vault.Vault{}).ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", path, rr.Code)
		}
	}
}
