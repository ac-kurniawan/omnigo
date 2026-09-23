package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestAddProvider(t *testing.T) {
	cfg := &config.Config{}
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	mutate := func(fn func(*config.Config) error) error {
		return fn(cfg)
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate)

	req := httptest.NewRequest("POST", "/providers", strings.NewReader("name=groq&type=openai&base_url=https://api.groq.com/openai/v1&api_key=gsk_123"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "groq" {
		t.Fatalf("providers = %+v", cfg.Providers)
	}
	if store.Get().Accounts("groq")[0].APIKey != "gsk_123" {
		t.Fatalf("secret = %+v", store.Get().Accounts("groq")[0])
	}
	if !strings.Contains(rr.Body.String(), "groq") {
		t.Fatalf("body missing groq: %s", rr.Body.String())
	}
}

func TestAddProviderNonHtmxRedirects(t *testing.T) {
	cfg := &config.Config{}
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{}})
	mutate := func(fn func(*config.Config) error) error {
		return fn(cfg)
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate)

	req := httptest.NewRequest("POST", "/providers", strings.NewReader("name=groq&type=openai&base_url=https://api.groq.com/openai/v1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/" {
		t.Fatalf("location = %q, want /", loc)
	}
}

func TestDeleteProvider(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "groq", Type: "openai", BaseURL: "https://api.groq.com"}},
	}
	store := vault.NewMemoryStore(&vault.Vault{
		ProviderSecrets: map[string]vault.ProviderSecret{"groq": {APIKey: "gsk_123"}},
	})
	mutate := func(fn func(*config.Config) error) error {
		return fn(cfg)
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/providers/groq/delete", nil)
	req.Header.Set("HX-Request", "true")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(cfg.Providers) != 0 {
		t.Fatalf("expected empty providers, got %+v", cfg.Providers)
	}
	if accounts := store.Get().Accounts("groq"); len(accounts) != 0 {
		t.Fatalf("expected secret deleted from vault: %+v", accounts)
	}
}

func TestSetProviderKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("providers:\n  - name: groq\n    type: openai\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "groq", Type: "openai", BaseURL: "https://api.groq.com"}},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		return config.Mutate(path, fn)
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate)

	req := httptest.NewRequest("POST", "/providers/groq/key", strings.NewReader("api_key=new_secret_key"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if store.Get().Accounts("groq")[0].APIKey != "new_secret_key" {
		t.Fatalf("vault key = %q, want new_secret_key", store.Get().Accounts("groq")[0].APIKey)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "new_secret_key") || strings.Contains(string(b), "api_key") {
		t.Fatalf("plaintext provider key written to config.yaml: %s", b)
	}
}

func TestRemoveOAuthAccountPreservesOtherAccounts(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex"}}}
	store := vault.NewMemoryStore(&vault.Vault{ProviderAccounts: map[string][]vault.ProviderSecret{
		"codex-main": {
			{AccountID: "workspace-1", AccessToken: "access-1"},
			{AccountID: "workspace-2", AccessToken: "access-2"},
		},
	}})
	s := newServer(func() *config.Config { return cfg }, store, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/providers/codex-main/accounts/workspace-1/delete", nil)
	req.Header.Set("HX-Request", "true")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got := store.Get().ProviderAccounts["codex-main"]
	if len(got) != 1 || got[0].AccountID != "workspace-2" {
		t.Fatalf("accounts = %+v", got)
	}
}

// Two users in the same workspace share AccountID; identity must include the
// user so only the targeted connection is removed.
func TestRemoveOAuthAccountDistinguishesUsersInSameWorkspace(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex"}}}
	store := vault.NewMemoryStore(&vault.Vault{ProviderAccounts: map[string][]vault.ProviderSecret{
		"codex-main": {
			{AccountID: "ws-1", Email: "alice@example.com", AccessToken: "access-alice"},
			{AccountID: "ws-1", Email: "bob@example.com", AccessToken: "access-bob"},
		},
	}})
	s := newServer(func() *config.Config { return cfg }, store, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/providers/codex-main/accounts/ws-1:alice@example.com/delete", nil)
	req.Header.Set("HX-Request", "true")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got := store.Get().Accounts("codex-main")
	if len(got) != 1 || got[0].Email != "bob@example.com" {
		t.Fatalf("accounts = %+v", got)
	}
}

func TestToggleProviderDisabled(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "groq", Type: "openai", BaseURL: "https://api.groq.com"}},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		return fn(cfg)
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate)

	req := httptest.NewRequest("POST", "/providers/groq/toggle", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !cfg.Providers[0].Disabled {
		t.Fatal("expected provider disabled after toggle")
	}
	if !strings.Contains(rr.Body.String(), "Disabled") {
		t.Fatalf("body missing Disabled badge: %s", rr.Body.String())
	}

	req = httptest.NewRequest("POST", "/providers/groq/toggle", nil)
	req.Header.Set("HX-Request", "true")
	rr = httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if cfg.Providers[0].Disabled {
		t.Fatal("expected provider enabled after second toggle")
	}
}

func TestUpdateProviderTimeouts(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "groq", Type: "openai"}},
	}
	mutate := func(fn func(*config.Config) error) error {
		if err := fn(cfg); err != nil {
			return err
		}
		return cfg.Validate()
	}
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), mutate)

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/providers/groq/timeouts", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		s.routes().ServeHTTP(rr, req)
		return rr
	}

	rr := post("timeout=30s&stream_timeout=2m")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if cfg.Providers[0].Timeout != "30s" || cfg.Providers[0].StreamTimeout != "2m" {
		t.Fatalf("timeouts = %q/%q, want 30s/2m", cfg.Providers[0].Timeout, cfg.Providers[0].StreamTimeout)
	}
	if !strings.Contains(rr.Body.String(), `value="30s"`) || !strings.Contains(rr.Body.String(), `value="2m"`) {
		t.Fatalf("edit modal missing prefilled timeouts: %s", rr.Body.String())
	}

	rr = post("timeout=&stream_timeout=")
	if rr.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if cfg.Providers[0].Timeout != "" || cfg.Providers[0].StreamTimeout != "" {
		t.Fatalf("timeouts not cleared: %+v", cfg.Providers[0])
	}

	rr = post("timeout=nope&stream_timeout=2m")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid timeout status = %d, want 400", rr.Code)
	}
	if cfg.Providers[0].Timeout != "" || cfg.Providers[0].StreamTimeout != "" {
		t.Fatalf("rejected timeout was stored: %+v", cfg.Providers[0])
	}
}

func TestUpdateServerTimeouts(t *testing.T) {
	cfg := &config.Config{}
	mutate := func(fn func(*config.Config) error) error {
		if err := fn(cfg); err != nil {
			return err
		}
		return cfg.Validate()
	}
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), mutate)

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/settings/timeouts", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		s.routes().ServeHTTP(rr, req)
		return rr
	}

	rr := post("timeout=45s&stream_timeout=5m")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if cfg.Server.Timeout != "45s" || cfg.Server.StreamTimeout != "5m" {
		t.Fatalf("server timeouts = %q/%q, want 45s/5m", cfg.Server.Timeout, cfg.Server.StreamTimeout)
	}
	if !strings.Contains(rr.Body.String(), `value="45s"`) || !strings.Contains(rr.Body.String(), `value="5m"`) {
		t.Fatalf("settings form missing prefilled timeouts: %s", rr.Body.String())
	}

	rr = post("timeout=&stream_timeout=0")
	if rr.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if cfg.Server.Timeout != "" || cfg.Server.StreamTimeout != "0" {
		t.Fatalf("server timeouts = %q/%q, want empty/0", cfg.Server.Timeout, cfg.Server.StreamTimeout)
	}

	rr = post("timeout=nope")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid timeout status = %d, want 400", rr.Code)
	}
	if cfg.Server.Timeout != "" || cfg.Server.StreamTimeout != "0" {
		t.Fatalf("rejected server timeout was stored: %+v", cfg.Server)
	}
}
