package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddModel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(mutateYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := AddModel(p, "openai-main", "gpt-4.5-preview"); err != nil {
		t.Fatalf("AddModel: %v", err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	models := cfg.Providers[0].Models
	if len(models) != 3 || models[2] != "gpt-4.5-preview" {
		t.Fatalf("models = %v", models)
	}
}
func TestAddModelNoDuplicate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(mutateYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := AddModel(p, "openai-main", "gpt-4o"); err != nil {
		t.Fatalf("AddModel: %v", err)
	}

	cfg, _ := Load(p)
	if len(cfg.Providers[0].Models) != 2 {
		t.Fatalf("expected 2 models, got %v", cfg.Providers[0].Models)
	}
}

func TestDisableAndEnableModel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(mutateYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Disable gpt-4o
	if err := DisableModel(p, "openai-main", "gpt-4o"); err != nil {
		t.Fatalf("DisableModel: %v", err)
	}

	cfg, _ := Load(p)
	prov := cfg.Providers[0]
	if len(prov.Models) != 1 || prov.Models[0] != "gpt-4o-mini" {
		t.Fatalf("expected gpt-4o-mini to stay active, got %v", prov.Models)
	}
	if len(prov.DisabledModels) != 1 || prov.DisabledModels[0] != "gpt-4o" {
		t.Fatalf("disabled models = %v", prov.DisabledModels)
	}
	if !prov.IsModelDisabled("gpt-4o") {
		t.Fatal("expected IsModelDisabled to be true")
	}

	// Enable gpt-4o back
	if err := EnableModel(p, "openai-main", "gpt-4o"); err != nil {
		t.Fatalf("EnableModel: %v", err)
	}

	cfg, _ = Load(p)
	prov = cfg.Providers[0]
	if len(prov.Models) != 2 || prov.Models[0] != "gpt-4o-mini" || prov.Models[1] != "gpt-4o" {
		t.Fatalf("active models = %v", prov.Models)
	}
	if len(prov.DisabledModels) != 0 {
		t.Fatalf("expected 0 disabled models, got %v", prov.DisabledModels)
	}
}

func TestDeleteModel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(mutateYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	_ = DisableModel(p, "openai-main", "gpt-4o")

	if err := DeleteModel(p, "openai-main", "gpt-4o-mini"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteModel(p, "openai-main", "gpt-4o"); err != nil {
		t.Fatal(err)
	}

	cfg, _ := Load(p)
	prov := cfg.Providers[0]
	if len(prov.Models) != 0 || len(prov.DisabledModels) != 0 {
		t.Fatalf("models = %v, disabled = %v", prov.Models, prov.DisabledModels)
	}
}

func TestSetModelsPreservesDisabledModels(t *testing.T) {
	c := &Config{
		Providers: []Provider{
			{
				Name:           "openai-main",
				Type:           "openai",
				Models:         []string{"gpt-4o"},
				DisabledModels: []string{"gpt-3.5-turbo"},
			},
		},
	}

	// Upstream returned both gpt-4o and gpt-3.5-turbo and gpt-4o-mini
	err := SetModels(c, "openai-main", []string{"gpt-4o", "gpt-3.5-turbo", "gpt-4o-mini"})
	if err != nil {
		t.Fatal(err)
	}

	prov := c.Providers[0]
	// gpt-3.5-turbo should NOT be in active Models because it's disabled
	for _, m := range prov.Models {
		if m == "gpt-3.5-turbo" {
			t.Fatalf("disabled model gpt-3.5-turbo was included in active models: %v", prov.Models)
		}
	}
	if len(prov.Models) != 2 {
		t.Fatalf("models = %v, want [gpt-4o, gpt-4o-mini]", prov.Models)
	}
	if len(prov.DisabledModels) != 1 || prov.DisabledModels[0] != "gpt-3.5-turbo" {
		t.Fatalf("disabled models = %v", prov.DisabledModels)
	}
}

func TestSetModelsDropsGoneComboTargets(t *testing.T) {
	c := &Config{
		Providers: []Provider{
			{Name: "openai-main", Type: "openai", Models: []string{"gpt-4o", "gpt-4o-mini"}},
			{Name: "other", Type: "openai", Models: []string{"keep-me"}},
		},
		Combos: []Combo{{
			Name:     "auto",
			Strategy: "priority",
			Targets: []ComboTarget{
				{Provider: "openai-main", Model: "gpt-4o"},
				{Provider: "openai-main", Model: "gone"},
				{Provider: "other", Model: "keep-me"},
			},
		}},
	}

	if err := SetModels(c, "openai-main", []string{"gpt-4o"}); err != nil {
		t.Fatal(err)
	}

	got := c.Combos[0].Targets
	want := []ComboTarget{
		{Provider: "openai-main", Model: "gpt-4o"},
		{Provider: "other", Model: "keep-me"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("targets = %+v, want %+v", got, want)
	}
}

func TestSetModelsDropsComboWhenEveryTargetGone(t *testing.T) {
	c := &Config{
		Providers: []Provider{
			{Name: "openai-main", Type: "openai", Models: []string{"gpt-4o"}},
		},
		Combos: []Combo{
			{
				Name:     "only-gone",
				Strategy: "priority",
				Targets:  []ComboTarget{{Provider: "openai-main", Model: "gpt-4o"}},
			},
			{
				Name:     "kept",
				Strategy: "priority",
				Targets:  []ComboTarget{{Provider: "openai-main", Model: "stays"}},
			},
		},
	}

	if err := SetModels(c, "openai-main", []string{"stays"}); err != nil {
		t.Fatal(err)
	}
	if len(c.Combos) != 1 || c.Combos[0].Name != "kept" {
		t.Fatalf("combos = %+v", c.Combos)
	}
}

func TestSetModelsKeepsTargetsWhenCatalogEmpty(t *testing.T) {
	c := &Config{
		Providers: []Provider{
			{Name: "openai-main", Type: "openai", Models: []string{}},
		},
		Combos: []Combo{{
			Name:     "auto",
			Strategy: "priority",
			Targets:  []ComboTarget{{Provider: "openai-main", Model: "gpt-4o"}},
		}},
	}

	if err := SetModels(c, "openai-main", nil); err != nil {
		t.Fatal(err)
	}
	if len(c.Combos[0].Targets) != 1 {
		t.Fatalf("empty catalog must not drop targets: %+v", c.Combos[0].Targets)
	}
}

func TestSetModelsDropsDisabledModelTargets(t *testing.T) {
	c := &Config{
		Providers: []Provider{{
			Name:           "openai-main",
			Type:           "openai",
			Models:         []string{"gpt-4o", "old"},
			DisabledModels: []string{"old"},
		}},
		Combos: []Combo{{
			Name:     "auto",
			Strategy: "priority",
			Targets: []ComboTarget{
				{Provider: "openai-main", Model: "gpt-4o"},
				{Provider: "openai-main", Model: "old"},
			},
		}},
	}

	if err := SetModels(c, "openai-main", []string{"gpt-4o", "old"}); err != nil {
		t.Fatal(err)
	}
	got := c.Combos[0].Targets
	if len(got) != 1 || got[0].Model != "gpt-4o" {
		t.Fatalf("disabled model must leave the combo: %+v", got)
	}
}

func TestSetProviderDisabled(t *testing.T) {
	c := &Config{
		Providers: []Provider{{Name: "openai-main", Type: "openai"}},
	}
	if err := SetProviderDisabled(c, "openai-main", true); err != nil {
		t.Fatalf("SetProviderDisabled: %v", err)
	}
	if !c.Providers[0].Disabled {
		t.Fatal("expected Disabled = true")
	}
	if err := SetProviderDisabled(c, "openai-main", false); err != nil {
		t.Fatalf("SetProviderDisabled: %v", err)
	}
	if c.Providers[0].Disabled {
		t.Fatal("expected Disabled = false")
	}
}

func TestSetProviderDisabledUnknown(t *testing.T) {
	c := &Config{}
	if err := SetProviderDisabled(c, "nope", true); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
