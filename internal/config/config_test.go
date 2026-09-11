package config

import (
	"os"
	"path/filepath"
	"testing"
)

const validYAML = `server:
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

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(validYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Fatalf("port = %d, want 8080", cfg.Server.Port)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "openai-main" {
		t.Fatalf("providers = %+v", cfg.Providers)
	}
	if len(cfg.Combos) != 1 || cfg.Combos[0].Strategy != "priority" {
		t.Fatalf("combos = %+v", cfg.Combos)
	}
}

func TestValidateRejectsUnknownProviderType(t *testing.T) {
	cfg := &Config{Providers: []Provider{{Name: "x", Type: "wat"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown provider type")
	}
}

func TestValidateRejectsUnknownStrategy(t *testing.T) {
	cfg := &Config{Combos: []Combo{{Name: "auto", Strategy: "weighted"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

func TestValidateAcceptsKnownValues(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{{Name: "a", Type: "openai"}, {Name: "b", Type: "antigravity"}},
		Combos:    []Combo{{Name: "auto", Strategy: "priority"}, {Name: "x", Strategy: "fill-first"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestLoadProviderAPIKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yamlContent := `providers:
  - name: my-openai
    type: openai
    api_key: sk-12345
`
	if err := os.WriteFile(p, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].APIKey != "sk-12345" {
		t.Fatalf("APIKey = %q, want sk-12345", cfg.Providers[0].APIKey)
	}
}
