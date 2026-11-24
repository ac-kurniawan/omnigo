package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// renderPlaygroundOptions renders the re-rendered model list fragment.
func renderPlaygroundOptions(t *testing.T, cfg *config.Config) string {
	t.Helper()
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest("GET", "/playground/models", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

// Each direct model advertises the reasoning levels it accepts so the
// playground's effort selector can offer exactly those, and a model that takes
// none advertises nothing.
func TestPlaygroundOptionsCarryEffortLevels(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.1-pro-low"}},
			{Name: "oai", Type: "openai", Models: []string{"gpt-4o"}},
		},
	}
	body := renderPlaygroundOptions(t, cfg)

	for _, want := range []string{
		`data-levels="low,medium,high"`,
		`data-levels=""`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("playground options missing %s: %s", want, body)
		}
	}
}

// The playground page ships the effort selector next to the model select and
// the JS that fills it, so a direct model can be dispatched at a chosen level.
func TestPlaygroundRendersEffortSelector(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.1-pro-low"}}},
	}
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()

	for _, want := range []string{
		`id="playground-effort"`,
		`onchange="renderPlaygroundEffort()"`,
		`function renderPlaygroundEffort()`,
		// The level must reach the gateway inside the model string.
		"`${model}|${effortLevel}`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("playground missing %q", want)
		}
	}
}
