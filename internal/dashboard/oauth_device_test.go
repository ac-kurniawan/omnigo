package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider/muse"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

type fakeMuseOAuth struct {
	devResp  *muse.DeviceCodeResponse
	devErr   error
	pollRes  muse.PollTokenResult
	pollErr  error
	mintResp *muse.KeyMintResponse
	mintErr  error
}

func (f *fakeMuseOAuth) RequestDeviceCode(ctx context.Context) (*muse.DeviceCodeResponse, error) {
	return f.devResp, f.devErr
}

func (f *fakeMuseOAuth) PollToken(ctx context.Context, deviceCode string) (muse.PollTokenResult, error) {
	return f.pollRes, f.pollErr
}

func (f *fakeMuseOAuth) MintSubscriptionKey(ctx context.Context, accessToken string) (*muse.KeyMintResponse, error) {
	return f.mintResp, f.mintErr
}

func TestMuseDeviceFlowStartAndPoll(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderAccounts: make(map[string][]vault.ProviderSecret)})
	cfg := &config.Config{Providers: []config.Provider{{Name: "meta-muse", Type: "muse"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)
	s.museClient = &fakeMuseOAuth{
		devResp:  &muse.DeviceCodeResponse{DeviceCode: "dev-999", UserCode: "USER-777", VerificationURI: "https://auth.meta.com/device", ExpiresIn: 300, Interval: 5},
		pollRes:  muse.PollTokenResult{AccessToken: "access-token-ok"},
		mintResp: &muse.KeyMintResponse{APIKey: "LLM|minted-key", UserEmail: "test@meta.com", SubsTierName: "Muse Code", IsSubsActive: true},
	}

	// Step 1: Start device flow without CSRF -> Rejected
	startReqNoCSRF := httptest.NewRequest("POST", "/providers/meta-muse/oauth/device/start", nil)
	recNoCSRF := httptest.NewRecorder()
	s.routes().ServeHTTP(recNoCSRF, startReqNoCSRF)
	if recNoCSRF.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on start without CSRF, got %d", recNoCSRF.Code)
	}

	// Step 2: Start with valid CSRF
	startReq := httptest.NewRequest("POST", "/providers/meta-muse/oauth/device/start", nil)
	startReq.AddCookie(&http.Cookie{Name: "omnigo_csrf", Value: "csrf-val"})
	startReq.Header.Set("X-Omnigo-CSRF", "csrf-val")
	startRec := httptest.NewRecorder()
	s.routes().ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK || !strings.Contains(startRec.Body.String(), "USER-777") {
		t.Fatalf("start code=%d body=%s", startRec.Code, startRec.Body.String())
	}

	cookies := startRec.Result().Cookies()
	var flowCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == deviceCookieName {
			flowCookie = c
			break
		}
	}
	if flowCookie == nil {
		t.Fatal("expected device flow session cookie")
	}

	// Step 3: Poll device flow with cookie
	pollReq := httptest.NewRequest("GET", "/providers/meta-muse/oauth/device/poll", nil)
	pollReq.AddCookie(flowCookie)
	pollRec := httptest.NewRecorder()
	s.routes().ServeHTTP(pollRec, pollReq)
	if pollRec.Code != http.StatusOK || !strings.Contains(pollRec.Body.String(), "Connected") {
		t.Fatalf("poll code=%d body=%s", pollRec.Code, pollRec.Body.String())
	}
	if strings.Contains(pollRec.Body.String(), "LLM|minted-key") {
		t.Fatal("poll response leaked secret API key")
	}

	// Check vault has updated key
	accounts := store.Get().Accounts("meta-muse")
	if len(accounts) == 0 || accounts[0].APIKey != "LLM|minted-key" || accounts[0].Email != "test@meta.com" {
		t.Fatalf("unexpected vault accounts: %+v", accounts)
	}
}

func TestMuseDeviceFlowConcurrentPollGuard(t *testing.T) {
	store := vault.NewMemoryStore(&vault.Vault{ProviderAccounts: make(map[string][]vault.ProviderSecret)})
	cfg := &config.Config{Providers: []config.Provider{{Name: "meta-muse", Type: "muse"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)

	s.oauthMu.Lock()
	flowID := "flow-123"
	s.devicePending[flowID] = devicePendingState{
		provider:   "meta-muse",
		flowID:     flowID,
		deviceCode: "dev-code",
		inflight:   true,
		expiresAt:  time.Now().Add(time.Minute),
		interval:   5,
	}
	s.oauthMu.Unlock()

	pollReq := httptest.NewRequest("GET", "/providers/meta-muse/oauth/device/poll", nil)
	pollReq.AddCookie(&http.Cookie{Name: deviceCookieName, Value: flowID})
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, pollReq)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Waiting for authorization") {
		t.Fatalf("expected non-racing response when inflight, got %s", rec.Body.String())
	}
}
