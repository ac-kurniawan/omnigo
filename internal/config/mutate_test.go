package config

import (
	"os"
	"path/filepath"
	"testing"
)

const mutateYAML = `server:
  host: 0.0.0.0
  port: 8080
providers:
  - name: openai-main
    type: openai
    base_url: https://api.openai.com/v1
    models: [gpt-4o, gpt-4o-mini]
combos:
  - name: auto
    strategy: priority
    targets:
      - provider: openai-main
        model: gpt-4o
`

func TestUpdateModelsPreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(mutateYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := UpdateModels(p, "openai-main", []string{"gpt-4o", "gpt-4o-mini"}); err != nil {
		t.Fatalf("UpdateModels: %v", err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Providers) != 1 || len(cfg.Providers[0].Models) != 2 {
		t.Fatalf("providers = %+v", cfg.Providers)
	}
	if cfg.Providers[0].Models[1] != "gpt-4o-mini" {
		t.Fatalf("models = %v", cfg.Providers[0].Models)
	}
	if cfg.Server.Port != 8080 {
		t.Fatalf("server lost: %+v", cfg.Server)
	}
	if len(cfg.Combos) != 1 || cfg.Combos[0].Strategy != "priority" {
		t.Fatalf("combos lost: %+v", cfg.Combos)
	}
}

func TestUpdateModelsUnknownProvider(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(mutateYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateModels(p, "nope", []string{"x"}); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestMutateValidates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(mutateYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Mutate(p, func(c *Config) error {
		c.Providers = append(c.Providers, Provider{Name: "bad", Type: "wat"})
		return nil
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestSetComboSuccess(t *testing.T) {
	cfg := &Config{
		Combos: []Combo{
			{
				Name:     "smart",
				Strategy: "priority",
				Targets: []ComboTarget{
					{Provider: "openai-main", Model: "gpt-4o"},
				},
			},
		},
	}

	newTargets := []ComboTarget{
		{Provider: "openai-main", Model: "gpt-4o-mini"},
		{Provider: "openai-main", Model: "gpt-4o"},
	}
	if err := SetCombo(cfg, "smart", "round-robin", newTargets, "", ""); err != nil {
		t.Fatalf("SetCombo: %v", err)
	}

	if cfg.Combos[0].Strategy != "round-robin" {
		t.Fatalf("strategy = %q, want round-robin", cfg.Combos[0].Strategy)
	}
	if len(cfg.Combos[0].Targets) != 2 || cfg.Combos[0].Targets[0].Model != "gpt-4o-mini" {
		t.Fatalf("targets = %+v", cfg.Combos[0].Targets)
	}
}

func TestSetComboUnknownCombo(t *testing.T) {
	cfg := &Config{Combos: []Combo{{Name: "smart", Strategy: "priority"}}}
	err := SetCombo(cfg, "nonexistent", "priority", []ComboTarget{{Provider: "p", Model: "m"}}, "", "")
	if err == nil {
		t.Fatal("expected error for unknown combo")
	}
}

func TestSetComboRejectsEmptyTargets(t *testing.T) {
	cfg := &Config{
		Combos: []Combo{
			{Name: "smart", Strategy: "priority", Targets: []ComboTarget{{Provider: "p", Model: "m"}}},
		},
	}
	err := SetCombo(cfg, "smart", "priority", nil, "", "")
	if err == nil {
		t.Fatal("expected error for empty targets")
	}
}

func TestSetComboUpdatesTimeoutAndDrainTTL(t *testing.T) {
	cfg := &Config{
		Combos: []Combo{{
			Name:     "smart",
			Strategy: "priority",
			Targets:  []ComboTarget{{Provider: "p", Model: "m"}},
			Timeout:  "90s",
			DrainTTL: "45s",
		}},
	}
	targets := []ComboTarget{{Provider: "p", Model: "m2"}}
	if err := SetCombo(cfg, "smart", "reliable", targets, "2m", "30s"); err != nil {
		t.Fatalf("SetCombo: %v", err)
	}
	got := cfg.Combos[0]
	if got.Timeout != "2m" {
		t.Fatalf("timeout = %q, want 2m", got.Timeout)
	}
	if got.DrainTTL != "30s" {
		t.Fatalf("drain_ttl = %q, want 30s", got.DrainTTL)
	}
	if err := SetCombo(cfg, "smart", "reliable", targets, "", ""); err != nil {
		t.Fatalf("SetCombo clear: %v", err)
	}
	if got := cfg.Combos[0]; got.Timeout != "" || got.DrainTTL != "" {
		t.Fatalf("budgets = timeout %q drain_ttl %q, want both cleared", got.Timeout, got.DrainTTL)
	}
}

func TestSetProviderTimeouts(t *testing.T) {
	cfg := &Config{Providers: []Provider{{Name: "groq", Type: "openai"}}}
	if err := SetProviderTimeouts(cfg, "groq", "30s", "2m"); err != nil {
		t.Fatalf("SetProviderTimeouts: %v", err)
	}
	p := cfg.Providers[0]
	if p.Timeout != "30s" || p.StreamTimeout != "2m" {
		t.Fatalf("provider timeouts = %q/%q, want 30s/2m", p.Timeout, p.StreamTimeout)
	}
	if err := SetProviderTimeouts(cfg, "groq", "", ""); err != nil {
		t.Fatalf("SetProviderTimeouts clear: %v", err)
	}
	if cfg.Providers[0].Timeout != "" || cfg.Providers[0].StreamTimeout != "" {
		t.Fatalf("provider timeouts not cleared: %+v", cfg.Providers[0])
	}
	if err := SetProviderTimeouts(cfg, "missing", "30s", ""); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestSetServerTimeouts(t *testing.T) {
	cfg := &Config{}
	if err := SetServerTimeouts(cfg, "45s", "5m"); err != nil {
		t.Fatalf("SetServerTimeouts: %v", err)
	}
	if cfg.Server.Timeout != "45s" || cfg.Server.StreamTimeout != "5m" {
		t.Fatalf("server timeouts = %q/%q, want 45s/5m", cfg.Server.Timeout, cfg.Server.StreamTimeout)
	}
	if err := SetServerTimeouts(cfg, "", ""); err != nil {
		t.Fatalf("SetServerTimeouts clear: %v", err)
	}
	if cfg.Server.Timeout != "" || cfg.Server.StreamTimeout != "" {
		t.Fatalf("server timeouts not cleared: %+v", cfg.Server)
	}
}

func TestUpdateComboDisk(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(mutateYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	newTargets := []ComboTarget{
		{Provider: "openai-main", Model: "gpt-4o"},
		{Provider: "openai-main", Model: "gpt-4o-mini"},
	}
	if err := UpdateCombo(p, "auto", "reliable", newTargets, "", ""); err != nil {
		t.Fatalf("UpdateCombo: %v", err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Combos) != 1 || cfg.Combos[0].Strategy != "reliable" {
		t.Fatalf("combo strategy = %q, want reliable", cfg.Combos[0].Strategy)
	}
	if len(cfg.Combos[0].Targets) != 2 || cfg.Combos[0].Targets[1].Model != "gpt-4o-mini" {
		t.Fatalf("combo targets = %+v", cfg.Combos[0].Targets)
	}
}
