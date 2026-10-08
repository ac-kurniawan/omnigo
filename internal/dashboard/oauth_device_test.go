package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	store := vault.NewMemoryStore(&vault.Vault{
		ProviderAccounts: make(map[string][]vault.ProviderSecret),
	})
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "meta-muse", Type: "muse"},
		},
	}

	s := newServer(func() *config.Config { return cfg }, store, nil)
	fake := &fakeMuseOAuth{
		devResp: &muse.DeviceCodeResponse{
			DeviceCode:      "dev-999",
			UserCode:        "USER-777",
			VerificationURI: "https://auth.meta.com/device",
			ExpiresIn:       300,
			Interval:        5,
		},
		pollRes: muse.PollTokenResult{
			AccessToken: "access-token-ok",
		},
		mintResp: &muse.KeyMintResponse{
			APIKey:       "LLM|minted-key",
			UserEmail:    "test@meta.com",
			SubsTierName: "Muse Code",
			IsSubsActive: true,
		},
	}
	s.museClient = fake

	// Step 1: Start device flow
	startReq := httptest.NewRequest("POST", "/providers/meta-muse/oauth/device/start", nil)
	rrStart := httptest.NewRecorder()
	s.routes().ServeHTTP(rrStart, startReq)

	if rrStart.Code != http.StatusOK {
		t.Fatalf("expected 200 on start, got %d: %s", rrStart.Code, rrStart.Body.String())
	}
	if !strings.Contains(rrStart.Body.String(), "USER-777") {
		t.Fatalf("expected user code in response, got %s", rrStart.Body.String())
	}

	// Step 2: Poll device flow
	pollReq := httptest.NewRequest("GET", "/providers/meta-muse/oauth/device/poll", nil)
	rrPoll := httptest.NewRecorder()
	s.routes().ServeHTTP(rrPoll, pollReq)

	if rrPoll.Code != http.StatusOK {
		t.Fatalf("expected 200 on poll, got %d: %s", rrPoll.Code, rrPoll.Body.String())
	}
	if !strings.Contains(rrPoll.Body.String(), "Connected") && !strings.Contains(rrPoll.Body.String(), "success") {
		t.Fatalf("expected success message in poll, got %s", rrPoll.Body.String())
	}

	// Check vault has updated key
	accounts := store.Get().Accounts("meta-muse")
	if len(accounts) == 0 {
		t.Fatal("expected account in vault")
	}
	if accounts[0].APIKey != "LLM|minted-key" {
		t.Errorf("APIKey=%s, want LLM|minted-key", accounts[0].APIKey)
	}
	if accounts[0].Email != "test@meta.com" {
		t.Errorf("Email=%s, want test@meta.com", accounts[0].Email)
	}
}
