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

// Every rendered model row must carry a well-formed data-target, and the effort
// badge must appear exactly once per clone row.
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
	clones := strings.Count(body, "picker-effort-item")
	if clones == 0 {
		t.Fatal("no clone rows rendered")
	}
	if badges := strings.Count(body, ">effort<"); badges != clones {
		t.Errorf("effort badge count %d != clone row count %d", badges, clones)
	}
}

// The end-to-end path must use the value the picker actually renders, not a
// hand-typed string, so a regression in the template is caught here.
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

	match := regexp.MustCompile(`data-target="(agy/gemini-3.1-pro-high)"`).FindStringSubmatch(pickRR.Body.String())
	if match == nil {
		t.Fatal("picker did not render the synthesised clone agy/gemini-3.1-pro-high")
	}
	target := match[1]

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
	if got := cfg.Combos[0].Targets[0].Provider + "/" + cfg.Combos[0].Targets[0].Model; got != target {
		t.Fatalf("stored target = %q, want the picker value %q", got, target)
	}
}
