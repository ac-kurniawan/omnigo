package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// renderComboPicker returns the HTML of the model picker for the given config.
func renderComboPicker(t *testing.T, cfg *config.Config) string {
	t.Helper()
	s := newServer(func() *config.Config { return cfg }, vault.NewMemoryStore(&vault.Vault{}), nil)
	req := httptest.NewRequest("GET", "/combo-picker/models", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

// The picker clones each effort-capable model and appends the suffix, so a user
// can pick an effort variant without a separate dropdown.
func TestComboPickerOffersEffortClones(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			// The catalog lists only the "-low" variant of this family, so the
			// "-medium" and "-high" clones are synthesised.
			{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.1-pro-low"}},
			{Name: "cx", Type: "codex", Models: []string{"gpt-5.5"}},
		},
	}
	body := renderComboPicker(t, cfg)

	for _, want := range []string{
		`data-target="agy/gemini-3.1-pro-low"`,    // the real catalog entry
		`data-target="agy/gemini-3.1-pro-medium"`, // synthesised clone
		`data-target="agy/gemini-3.1-pro-high"`,   // synthesised clone
		`data-target="cx/gpt-5.5-low"`,            // codex clone from its profile
		`data-target="cx/gpt-5.5-xhigh"`,          // the top codex level
	} {
		if !strings.Contains(body, want) {
			t.Errorf("picker missing %s", want)
		}
	}
}

// A model that cannot take an effort must not grow clones.
func TestComboPickerSkipsUncloneableModels(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "oai", Type: "openai", Models: []string{"gpt-4o"}},
			{Name: "agy", Type: "antigravity", Models: []string{"claude-sonnet-4-6"}},
		},
	}
	body := renderComboPicker(t, cfg)

	for _, unwanted := range []string{
		`data-target="oai/gpt-4o-high"`,
		`data-target="oai/gpt-4o-medium"`,
		`data-target="agy/claude-sonnet-4-6-high"`,
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("picker should not offer %s", unwanted)
		}
	}
	if !strings.Contains(body, `data-target="oai/gpt-4o"`) {
		t.Error("picker dropped the plain model")
	}
}

func TestComboPickerListsEachModelOnce(t *testing.T) {
	// The catalog already lists every level of this family, so the clones would
	// duplicate the real entries. Each id must appear exactly once.
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.7-flash-low", "gemini-3.7-flash-medium", "gemini-3.7-flash-high"}},
		},
	}
	body := renderComboPicker(t, cfg)

	for _, id := range []string{"gemini-3.7-flash-low", "gemini-3.7-flash-medium", "gemini-3.7-flash-high"} {
		target := `data-target="agy/` + id + `"`
		if got := strings.Count(body, target); got != 1 {
			t.Errorf("%s appears %d times, want exactly 1", target, got)
		}
	}
	// The same catalog entry also must not show as a badge-carrying clone.
	if got := strings.Count(body, `data-model="gemini-3.7-flash-high"`); got != 1 {
		t.Errorf("gemini-3.7-flash-high rendered %d times, want 1", got)
	}
}

// A clone must be visibly marked as an effort variant so it is not mistaken for
// a distinct upstream model.
func TestComboPickerMarksEffortClones(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "cx", Type: "codex", Models: []string{"gpt-5.5"}}},
	}
	body := renderComboPicker(t, cfg)

	if !strings.Contains(body, "picker-effort-item") {
		t.Error("effort clone is not marked")
	}
	if !strings.Contains(body, ">effort<") {
		t.Error("effort clone does not carry an effort badge")
	}
}
