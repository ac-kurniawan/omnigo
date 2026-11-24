package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// Every rendered model row must carry a well-formed data-target, and an
// effort-capable row must offer its levels as a dropdown.
func TestComboPickerRowsAreWellFormed(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "cx", Type: "codex", Models: []string{"gpt-5.5"}}},
	}
	body := renderComboPicker(t, cfg)

	row := regexp.MustCompile(`data-target="([^"]+)" data-provider="([^"]+)" data-model="([^"]+)"`)
	rows := row.FindAllStringSubmatch(body, -1)
	if len(rows) == 0 {
		t.Fatal("picker rendered no model rows")
	}
	for _, m := range rows {
		if m[1] != m[2]+"/"+m[3] {
			t.Errorf("data-target %q does not match provider/model %s/%s", m[1], m[2], m[3])
		}
	}
	if got := strings.Count(body, "picker-effort-select"); got != 1 {
		t.Errorf("effort dropdowns = %d, want one for gpt-5.5", got)
	}
	for _, level := range []string{`<option value="low">`, `<option value="xhigh">`} {
		if !strings.Contains(body, level) {
			t.Errorf("effort dropdown missing %s", level)
		}
	}
}

// The end-to-end path must use the encoding the picker actually produces: a
// checked model plus the row's dropdown yields "provider/model|effort", which
// the create-combo form turns into a stored base+effort target.
func TestCreateComboWithATargetTakenFromThePickerDOM(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.1-pro-low"}},
		},
	}
	store := vault.NewMemoryStore(&vault.Vault{})
	mutate := func(fn func(*config.Config) error) error {
		if err := fn(cfg); err != nil {
			return err
		}
		return cfg.Validate()
	}
	s := newServer(func() *config.Config { return cfg }, store, mutate, nil)

	pick := httptest.NewRequest("GET", "/combo-picker/models", nil)
	pick.Header.Set("HX-Request", "true")
	pickRR := httptest.NewRecorder()
	s.routes().ServeHTTP(pickRR, pick)
	if pickRR.Code != http.StatusOK {
		t.Fatalf("picker status = %d", pickRR.Code)
	}

	match := regexp.MustCompile(`data-target="(agy/gemini-3\.1-pro)"`).FindStringSubmatch(pickRR.Body.String())
	if match == nil {
		t.Fatal("picker did not render the gemini-3.1-pro row")
	}
	// What applyComboPickerSelection pushes for that row at "high".
	target := match[1] + "|high"

	form := url.Values{"name": {"smart"}, "strategy": {"priority"}, "targets": {target}}
	req := httptest.NewRequest("POST", "/combos", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if len(cfg.Combos) != 1 || len(cfg.Combos[0].Targets) != 1 {
		t.Fatalf("combos = %+v, want one target", cfg.Combos)
	}
	got := cfg.Combos[0].Targets[0]
	if got.Provider != "agy" || got.Model != "gemini-3.1-pro" || got.Effort != "high" {
		t.Fatalf("stored target = %+v, want agy/gemini-3.1-pro at high", got)
	}
	if !strings.Contains(rr.Body.String(), "gemini-3.1-pro") || !strings.Contains(rr.Body.String(), "effort high") {
		t.Fatalf("re-rendered combos missing the model or its effort badge: %s", rr.Body.String())
	}
}
