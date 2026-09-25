package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDashboardBasicAuthDisabled(t *testing.T) {
	mw := DashboardBasicAuth(func() bool { return false })
	nextCalled := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rr, req)

	if !nextCalled {
		t.Fatal("expected next handler to be called when auth is disabled")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestDashboardBasicAuthRejectsUnsetCredentials(t *testing.T) {
	t.Setenv("OMNIGO_DASH_USER", "")
	t.Setenv("OMNIGO_DASH_PASS", "")
	t.Setenv("OMNIGO_DASHBOARD_USER", "")
	t.Setenv("OMNIGO_DASHBOARD_PASS", "")
	t.Setenv("OMNIGO_DASH_PASSWORD", "")
	t.Setenv("OMNIGO_DASHBOARD_PASSWORD", "")

	if err := DashboardCredentials(); err == nil {
		t.Fatal("unset dashboard credentials were accepted")
	}

	mw := DashboardBasicAuth(func() bool { return true })
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "admin")
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("admin/admin with no credentials configured: status = %d, want 401", rr.Code)
	}
	if authHeader := rr.Header().Get("WWW-Authenticate"); authHeader != `Basic realm="OmniGo Dashboard"` {
		t.Fatalf("WWW-Authenticate = %q, want Basic realm=\"OmniGo Dashboard\"", authHeader)
	}
}

func TestDashboardBasicAuthCustomEnv(t *testing.T) {
	t.Setenv("OMNIGO_DASH_USER", "custom_user")
	t.Setenv("OMNIGO_DASH_PASS", "s3cret_p@ss")

	mw := DashboardBasicAuth(func() bool { return true })
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// 1. admin/admin should now fail
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "admin")
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("admin/admin with custom env: status = %d, want 401", rr.Code)
	}

	// 2. custom_user / s3cret_p@ss should succeed
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("custom_user", "s3cret_p@ss")
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("custom credentials: status = %d, want 200", rr.Code)
	}
}

func TestDashboardBasicAuthBypassesStatic(t *testing.T) {
	mw := DashboardBasicAuth(func() bool { return true })
	nextCalled := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/htmx.min.js", nil)
	handler.ServeHTTP(rr, req)

	if !nextCalled {
		t.Fatal("expected static path to bypass basic auth")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}
