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

// The picker shows one row per model and offers that model's reasoning levels
// as a dropdown next to the name, instead of a cloned row per level.
func TestComboPickerOffersEffortLevelsPerModel(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			// The catalog lists only the "-low" variant of this family, so the
			// row is the base and the dropdown carries every level.
			{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.1-pro-low"}},
			{Name: "cx", Type: "codex", Models: []string{"gpt-5.5"}},
		},
	}
	body := renderComboPicker(t, cfg)

	for _, want := range []string{`data-target="agy/gemini-3.1-pro"`, `data-target="cx/gpt-5.5"`} {
		if !strings.Contains(body, want) {
			t.Errorf("picker missing the model row %s", want)
		}
	}
	for _, unwanted := range []string{
		`data-target="agy/gemini-3.1-pro-low"`,
		`data-target="agy/gemini-3.1-pro-high"`,
		`data-target="cx/gpt-5.5-low"`,
		`data-target="cx/gpt-5.5-xhigh"`,
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("picker still clones the effort level: %s", unwanted)
		}
	}
	if got := strings.Count(body, "picker-effort-select"); got != 2 {
		t.Errorf("effort dropdowns = %d, want one per effort-capable model (2)", got)
	}
	for _, level := range []string{`<option value="low">`, `<option value="medium">`, `<option value="high">`, `<option value="xhigh">`} {
		if !strings.Contains(body, level) {
			t.Errorf("effort dropdown missing %s", level)
		}
	}
}

// A model that cannot take an effort gets no dropdown.
func TestComboPickerSkipsUncloneableModels(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "oai", Type: "openai", Models: []string{"gpt-4o"}},
			{Name: "agy", Type: "antigravity", Models: []string{"claude-sonnet-4-6"}},
		},
	}
	body := renderComboPicker(t, cfg)

	if strings.Contains(body, "picker-effort-select") {
		t.Error("picker offered an effort dropdown for a model that takes none")
	}
	for _, want := range []string{`data-target="oai/gpt-4o"`, `data-target="agy/claude-sonnet-4-6"`} {
		if !strings.Contains(body, want) {
			t.Errorf("picker dropped the plain model %s", want)
		}
	}
}

// A provider type that fronts an upstream taking effort in the body still
// offers its model's levels, including a namespaced model id.
func TestComboPickerOffersEffortForOpencodeAndCommandcode(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "oc", Type: "opencode", Models: []string{"deepseek-v4.1-flash"}},
			{Name: "ccm", Type: "commandcode", Models: []string{"deepseek/deepseek-v4-pro"}},
		},
	}
	body := renderComboPicker(t, cfg)

	for _, want := range []string{`data-target="oc/deepseek-v4.1-flash"`, `data-target="ccm/deepseek/deepseek-v4-pro"`} {
		if !strings.Contains(body, want) {
			t.Errorf("picker missing the model row %s", want)
		}
	}
	if got := strings.Count(body, "picker-effort-select"); got != 2 {
		t.Errorf("effort dropdowns = %d, want one per effort-capable model (2)", got)
	}
	for _, level := range []string{`<option value="low">`, `<option value="medium">`, `<option value="high">`} {
		if !strings.Contains(body, level) {
			t.Errorf("effort dropdown missing %s", level)
		}
	}
}

// A family whose catalog lists every level collapses into a single row on its
// base, so the same model never appears twice.
func TestComboPickerListsEachModelOnce(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.7-flash-low", "gemini-3.7-flash-medium", "gemini-3.7-flash-high"}},
		},
	}
	body := renderComboPicker(t, cfg)

	if got := strings.Count(body, `data-target="agy/gemini-3.7-flash"`); got != 1 {
		t.Errorf("agy/gemini-3.7-flash appears %d times, want exactly 1", got)
	}
	for _, id := range []string{"gemini-3.7-flash-low", "gemini-3.7-flash-medium", "gemini-3.7-flash-high"} {
		if strings.Contains(body, `data-target="agy/`+id+`"`) {
			t.Errorf("level variant %s still has its own row", id)
		}
	}
}
