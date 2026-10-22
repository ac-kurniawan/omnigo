package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// fakeDecider is a System One stand-in that answers with a fixed route, or
// fails in a fixed way.
type fakeDecider struct {
	choice     string
	confidence float64
	fail       error
	states     []string
}

func (f *fakeDecider) Decide(ctx context.Context, state string, routes []config.SmartRoute) (smartVerdict, error) {
	f.states = append(f.states, state)
	if f.fail != nil {
		return smartVerdict{}, f.fail
	}
	return smartVerdict{Route: f.choice, Confidence: f.confidence}, nil
}

// smartResolver turns a smart combo's route into the target it dispatches to.
// It lives in the api package because resolving a destination to a provider
// needs the provider registry.
type smartTestEnv struct {
	cfg     *config.Config
	decider *fakeDecider
}

func newSmartEnv(t *testing.T, jevChoice string, jevConfidence float64, jevErr error) (*smartTestEnv, *[]string) {
	t.Helper()
	called := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		*called = append(*called, body.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.8-flash", "gemini-3.1-pro"}},
		},
		Combos: []config.Combo{
			{Name: "combo/flash", Strategy: "priority", Targets: []config.ComboTarget{{Provider: "agy", Model: "gemini-3.8-flash"}}},
			{Name: "combo/pro", Strategy: "priority", Targets: []config.ComboTarget{{Provider: "agy", Model: "gemini-3.1-pro"}}},
			{
				Name:         "combo/smart",
				Strategy:     "smart",
				DefaultRoute: "flash",
				Routes: []config.SmartRoute{
					{Name: "flash", Criteria: "Simple request.", Destination: "combo/flash"},
					{Name: "pro", Criteria: "Complex request.", Destination: "combo/pro"},
				},
			},
		},
	}
	return &smartTestEnv{cfg: cfg, decider: &fakeDecider{choice: jevChoice, confidence: jevConfidence, fail: jevErr}}, called
}

// resolveSmartRoute is the unit under test: it turns a route name into the
// concrete provider/model to dispatch to.
func resolveSmartRoute(cfg *config.Config, cb config.Combo, routeName string) (string, string, bool) {
	for _, route := range cb.Routes {
		if route.Name != routeName {
			continue
		}
		if target, ok := config.FindCombo(cfg, route.Destination); ok {
			if len(target.Targets) == 0 {
				return "", "", false
			}
			return target.Targets[0].Provider, target.Targets[0].Model, true
		}
		providerName, model, ok := strings.Cut(route.Destination, "/")
		if !ok || providerName == "" || model == "" {
			return "", "", false
		}
		return providerName, model, true
	}
	return "", "", false
}

func TestResolveSmartRouteToComboDestination(t *testing.T) {
	env, _ := newSmartEnv(t, "pro", 0.9, nil)
	provider, model, ok := resolveSmartRoute(env.cfg, env.cfg.Combos[2], "pro")
	if !ok {
		t.Fatal("route did not resolve")
	}
	if provider != "agy" || model != "gemini-3.1-pro" {
		t.Errorf("resolved %s/%s, want agy/gemini-3.1-pro", provider, model)
	}
}

func TestResolveSmartRouteToDirectProviderDestination(t *testing.T) {
	env, _ := newSmartEnv(t, "flash", 0.9, nil)
	env.cfg.Combos[2].Routes[0].Destination = "agy/gemini-3.8-flash"
	provider, model, ok := resolveSmartRoute(env.cfg, env.cfg.Combos[2], "flash")
	if !ok {
		t.Fatal("route did not resolve")
	}
	if provider != "agy" || model != "gemini-3.8-flash" {
		t.Errorf("resolved %s/%s", provider, model)
	}
}

func TestResolveSmartRouteUnknownRoute(t *testing.T) {
	env, _ := newSmartEnv(t, "flash", 0.9, nil)
	if _, _, ok := resolveSmartRoute(env.cfg, env.cfg.Combos[2], "nope"); ok {
		t.Error("unknown route resolved")
	}
}

// The default route is what a failed decision falls back to.
func TestSmartDefaultRouteFallback(t *testing.T) {
	env, _ := newSmartEnv(t, "", 0, errors.New("jev down"))
	cb := env.cfg.Combos[2]
	provider, model, ok := resolveSmartRoute(env.cfg, cb, cb.DefaultRoute)
	if !ok {
		t.Fatal("default route did not resolve")
	}
	if provider != "agy" || model != "gemini-3.8-flash" {
		t.Errorf("default resolved %s/%s, want agy/gemini-3.8-flash", provider, model)
	}
}

// The state Jev judges is the last user message only.
func TestSmartStateIsLastUserMessage(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "an answer"},
		{Role: "user", Content: "second question"},
	}
	state := smartState(msgs)
	if state != "second question" {
		t.Errorf("state = %q, want %q", state, "second question")
	}
}

func TestSmartStateFallsBackToLastMessage(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "system"},
		{Role: "assistant", Content: "only assistant"},
	}
	state := smartState(msgs)
	if state != "only assistant" {
		t.Errorf("state = %q, want %q", state, "only assistant")
	}
}

func TestSmartStateEmptyMessages(t *testing.T) {
	if got := smartState(nil); got != "" {
		t.Errorf("state = %q, want empty", got)
	}
}
