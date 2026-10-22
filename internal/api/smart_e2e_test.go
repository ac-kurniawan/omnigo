package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/systemone"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// jevServer is a TypeSafe stand-in that answers the choice question with a
// configured route, and records what state it was asked about.
func systemOneServer(t *testing.T, choice string, confidence float64, gotState *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			State     string `json:"state"`
			Questions map[string]struct {
				Type     string            `json:"type"`
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(raw, &body)
		if gotState != nil {
			*gotState = body.State
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-1.13.0",
			"answers": map[string]any{
				"route": map[string]any{"type": "choice", "choice": choice, "confidence": confidence},
			},
			"usage": map[string]any{"input_tokens": 12, "output_tokens": 4},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// smartConfig is a config with a plain flash combo, a plain pro combo, and a
// smart combo that routes between them.
func smartConfig() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{"gpt-4o", "gpt-4o-mini"}},
		},
		Combos: []config.Combo{
			{Name: "combo/flash", Strategy: "priority", Targets: []config.ComboTarget{{Provider: "openai", Model: "gpt-4o-mini"}}},
			{Name: "combo/pro", Strategy: "priority", Targets: []config.ComboTarget{{Provider: "openai", Model: "gpt-4o"}}},
			{
				Name:         "combo/smart",
				Strategy:     config.StrategySmart,
				DefaultRoute: "flash",
				Routes: []config.SmartRoute{
					{Name: "flash", Criteria: "A simple request.", Destination: "combo/flash"},
					{Name: "pro", Criteria: "A complex request.", Destination: "combo/pro"},
				},
			},
		},
	}
}

// useSystemOneServer points the smart combo's System One client at a test
// server.
func useSystemOneServer(t *testing.T, baseURL string) {
	t.Helper()
	prev := systemOneClientFactoryOverride
	systemOneClientFactoryOverride = func(cfg config.SmartDecision) smartDecider {
		return newSystemOneClientForTest(cfg, baseURL)
	}
	t.Cleanup(func() { systemOneClientFactoryOverride = prev })
}

// newSystemOneClientForTest builds a System One client pointed at a local
// stand-in server. The API key is a placeholder: the stand-in does not check
// it.
func newSystemOneClientForTest(cfg config.SmartDecision, baseURL string) smartDecider {
	client := systemone.New(systemone.Config{
		BaseURL: baseURL,
		Model:   cfg.ModelOrDefault(),
		APIKey:  "test-key",
		Timeout: cfg.ParsedTimeout(),
	})
	return newSystemOneDecider(client, cfg)
}

// A request through a smart combo reaches the provider the chosen route names.
func TestSmartComboE2ERoutesToChosenDestination(t *testing.T) {
	var gotModel string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, chat: func(r provider.ChatRequest) error {
			gotModel = r.Model
			return nil
		}}
	})
	srv := systemOneServer(t, "pro", 0.9, nil)
	useSystemOneServer(t, srv.URL)

	cfg := smartConfig()
	rawKey, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"combo/smart","messages":[{"role":"user","content":"do a hard thing"}]}`))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	if gotModel != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o (the pro route)", gotModel)
	}
}

// The state Jev sees is the last user message only.
func TestSmartComboE2ESendsLastUserMessageToJev(t *testing.T) {
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, chat: func(r provider.ChatRequest) error { return nil }}
	})
	var gotState string
	srv := systemOneServer(t, "flash", 0.9, &gotState)
	useSystemOneServer(t, srv.URL)

	cfg := smartConfig()
	rawKey, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}

	body := `{"model":"combo/smart","messages":[{"role":"system","content":"be helpful"},{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"the real question"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	if gotState != "the real question" {
		t.Errorf("jev state = %q, want %q", gotState, "the real question")
	}
}

// When Jev fails, the default route is used and the request still succeeds.
func TestSmartComboE2EFallsBackToDefaultRoute(t *testing.T) {
	var gotModel string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, chat: func(r provider.ChatRequest) error {
			gotModel = r.Model
			return nil
		}}
	})
	// A server that always fails: the decision cannot succeed.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	useSystemOneServer(t, failing.URL)

	cfg := smartConfig()
	rawKey, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"combo/smart","messages":[{"role":"user","content":"anything"}]}`))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	if gotModel != "gpt-4o-mini" {
		t.Errorf("model = %q, want gpt-4o-mini (the default route)", gotModel)
	}
}

// A low-confidence answer is treated as a failure and takes the default route.
func TestSmartComboE2ELowConfidenceUsesDefaultRoute(t *testing.T) {
	var gotModel string
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name, chat: func(r provider.ChatRequest) error {
			gotModel = r.Model
			return nil
		}}
	})
	srv := systemOneServer(t, "pro", 0.1, nil)
	useSystemOneServer(t, srv.URL)

	cfg := smartConfig()
	rawKey, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"combo/smart","messages":[{"role":"user","content":"anything"}]}`))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
	}
	if gotModel != "gpt-4o-mini" {
		t.Errorf("model = %q, want gpt-4o-mini (default route after low confidence)", gotModel)
	}
}

// The smart combo is listed as a model, like any other combo.
func TestSmartComboAppearsInModels(t *testing.T) {
	rawKey, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rr := httptest.NewRecorder()
	testRouter(t, smartConfig(), v).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"id":"combo/smart"`) {
		t.Errorf("models missing combo/smart: %s", rr.Body.String())
	}
}
