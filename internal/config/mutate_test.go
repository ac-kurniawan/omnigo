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
    models: [gpt-4o]
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
