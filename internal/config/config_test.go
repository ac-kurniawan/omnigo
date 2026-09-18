package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
		Providers: []Provider{{Name: "a", Type: "openai"}, {Name: "b", Type: "antigravity"}, {Name: "c", Type: "codex"}},
		Combos: []Combo{
			{Name: "auto", Strategy: "priority"},
			{Name: "x", Strategy: "fill-first", DrainTTL: "30s"},
			{Name: "safe", Strategy: "reliable", DrainTTL: "30s"},
			{Name: "balanced", Strategy: "round-robin", DrainTTL: "30s"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsInvalidDrainTTL(t *testing.T) {
	cfg := &Config{
		Combos: []Combo{{Name: "x", Strategy: "fill-first", DrainTTL: "invalid-duration"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid drain_ttl duration")
	}
}

func TestProviderAPIKeyIsNotSerialized(t *testing.T) {
	cfg := Config{Providers: []Provider{{Name: "my-openai", Type: "openai", APIKey: "sk-12345"}}}
	b, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sk-12345") || strings.Contains(string(b), "api_key") {
		t.Fatalf("serialized config contains API key: %s", b)
	}
}

func TestLoadProviderDisabled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yamlContent := `providers:
  - name: groq
    type: openai
    disabled: true
`
	if err := os.WriteFile(p, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || !cfg.Providers[0].Disabled {
		t.Fatalf("Disabled = %v, want true", cfg.Providers[0].Disabled)
	}
}

func TestTimeoutDefaultIs20s(t *testing.T) {
	cfg := &Config{}
	if got := cfg.DefaultTimeout(); got != 20*time.Second {
		t.Fatalf("DefaultTimeout = %v, want 20s", got)
	}
	if got := cfg.Server.ParsedTimeout(); got != 20*time.Second {
		t.Fatalf("Server.ParsedTimeout = %v, want 20s", got)
	}
	p := Provider{Name: "test", Type: "openai"}
	if got := p.ParsedTimeout(cfg.DefaultTimeout()); got != 20*time.Second {
		t.Fatalf("Provider.ParsedTimeout = %v, want 20s", got)
	}
}

func TestStreamTimeoutDefaultsAndOverrides(t *testing.T) {
	cfg := &Config{}
	if got := cfg.DefaultStreamTimeout(); got != 10*time.Minute {
		t.Fatalf("DefaultStreamTimeout = %v, want 10m", got)
	}
	if got := cfg.Server.ParsedStreamTimeout(); got != 10*time.Minute {
		t.Fatalf("Server.ParsedStreamTimeout = %v, want 10m", got)
	}
	p := Provider{Name: "test", Type: "openai"}
	if got := p.ParsedStreamTimeout(cfg.DefaultStreamTimeout()); got != 10*time.Minute {
		t.Fatalf("Provider.ParsedStreamTimeout = %v, want 10m", got)
	}

	cfgCustom := &Config{
		Server: Server{StreamTimeout: "5m"},
		Providers: []Provider{
			{Name: "custom", Type: "openai", StreamTimeout: "30m"},
			{Name: "unbounded", Type: "openai", StreamTimeout: "0s"},
			{Name: "fallback", Type: "openai"},
		},
	}
	if got := cfgCustom.DefaultStreamTimeout(); got != 5*time.Minute {
		t.Fatalf("Server custom stream timeout = %v, want 5m", got)
	}
	if got := cfgCustom.Providers[0].ParsedStreamTimeout(cfgCustom.DefaultStreamTimeout()); got != 30*time.Minute {
		t.Fatalf("custom provider stream timeout = %v, want 30m", got)
	}
	if got := cfgCustom.Providers[1].ParsedStreamTimeout(cfgCustom.DefaultStreamTimeout()); got != 0 {
		t.Fatalf("unbounded provider stream timeout = %v, want 0", got)
	}
	if got := cfgCustom.Providers[2].ParsedStreamTimeout(cfgCustom.DefaultStreamTimeout()); got != 5*time.Minute {
		t.Fatalf("fallback provider stream timeout = %v, want 5m", got)
	}

	cfgServerZero := &Config{
		Server: Server{StreamTimeout: "0s"},
		Providers: []Provider{
			{Name: "server-unbounded", Type: "openai"},
		},
	}
	if got := cfgServerZero.DefaultStreamTimeout(); got != 0 {
		t.Fatalf("Server 0s stream timeout = %v, want 0", got)
	}
	if got := cfgServerZero.Providers[0].ParsedStreamTimeout(cfgServerZero.DefaultStreamTimeout()); got != 0 {
		t.Fatalf("inherited 0s stream timeout = %v, want 0", got)
	}
}

func TestTimeoutServerConfigured(t *testing.T) {
	cfg := &Config{
		Server: Server{Timeout: "45s"},
	}
	if got := cfg.DefaultTimeout(); got != 45*time.Second {
		t.Fatalf("DefaultTimeout = %v, want 45s", got)
	}
	p := Provider{Name: "test", Type: "openai"}
	if got := p.ParsedTimeout(cfg.DefaultTimeout()); got != 45*time.Second {
		t.Fatalf("Provider.ParsedTimeout = %v, want 45s", got)
	}
}

func TestTimeoutProviderOverride(t *testing.T) {
	cfg := &Config{
		Server: Server{Timeout: "20s"},
		Providers: []Provider{
			{Name: "fast", Type: "openai", Timeout: "5s"},
			{Name: "normal", Type: "openai"},
		},
	}
	if got := cfg.Providers[0].ParsedTimeout(cfg.DefaultTimeout()); got != 5*time.Second {
		t.Fatalf("fast provider timeout = %v, want 5s", got)
	}
	if got := cfg.Providers[1].ParsedTimeout(cfg.DefaultTimeout()); got != 20*time.Second {
		t.Fatalf("normal provider timeout = %v, want 20s", got)
	}
}

func TestValidateRejectsInvalidTimeout(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
	}{
		{"invalid server timeout", &Config{Server: Server{Timeout: "invalid"}}},
		{"negative server timeout", &Config{Server: Server{Timeout: "-10s"}}},
		{"invalid top-level timeout", &Config{Timeout: "invalid"}},
		{"invalid provider timeout", &Config{Providers: []Provider{{Name: "p", Type: "openai", Timeout: "invalid"}}}},
		{"invalid server stream timeout", &Config{Server: Server{StreamTimeout: "invalid"}}},
		{"negative server stream timeout", &Config{Server: Server{StreamTimeout: "-1s"}}},
		{"invalid provider stream timeout", &Config{Providers: []Provider{{Name: "p", Type: "openai", StreamTimeout: "invalid"}}}},
		{"negative provider stream timeout", &Config{Providers: []Provider{{Name: "p", Type: "openai", StreamTimeout: "-5s"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); err == nil {
				t.Errorf("%s: expected validation error", tt.name)
			}
		})
	}
}

func TestDashboardAuthEnabledDefault(t *testing.T) {
	cfg := &Config{}
	if !cfg.Dashboard.AuthEnabled() {
		t.Fatal("expected dashboard auth enabled by default (nil)")
	}
}

func TestDashboardAuthExplicitlyDisabled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yamlContent := `dashboard:
  auth: false
providers: []
combos: []
`
	if err := os.WriteFile(p, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Dashboard.AuthEnabled() {
		t.Fatal("expected dashboard auth disabled when explicitly false")
	}
}

func TestDashboardAuthExplicitlyEnabled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yamlContent := `dashboard:
  auth: true
providers: []
combos: []
`
	if err := os.WriteFile(p, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Dashboard.AuthEnabled() {
		t.Fatal("expected dashboard auth enabled when explicitly true")
	}
}

func TestDashboardEnabledDefault(t *testing.T) {
	cfg := &Config{}
	if !cfg.Dashboard.IsEnabled() {
		t.Fatal("expected dashboard enabled by default (nil)")
	}
}

func TestDashboardExplicitlyDisabled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yamlContent := `dashboard:
  enabled: false
providers: []
combos: []
`
	if err := os.WriteFile(p, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Dashboard.IsEnabled() {
		t.Fatal("expected dashboard disabled when explicitly false")
	}
}

func TestDashboardExplicitlyEnabled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yamlContent := `dashboard:
  enabled: true
providers: []
combos: []
`
	if err := os.WriteFile(p, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Dashboard.IsEnabled() {
		t.Fatal("expected dashboard enabled when explicitly true")
	}
}

func TestObservabilityMetricsDisabledByDefault(t *testing.T) {
	cfg := &Config{}
	if cfg.Observability.MetricsEnabled() {
		t.Fatal("expected metrics disabled by default (nil)")
	}
}

func TestObservabilityMetricsExplicitlyDisabled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yamlContent := `observability:
  metrics: false
providers: []
combos: []
`
	if err := os.WriteFile(p, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Observability.MetricsEnabled() {
		t.Fatal("expected metrics disabled when explicitly false")
	}
}

func TestObservabilityMetricsExplicitlyEnabled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	yamlContent := `observability:
  metrics: true
providers: []
combos: []
`
	if err := os.WriteFile(p, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Observability.MetricsEnabled() {
		t.Fatal("expected metrics enabled when explicitly true")
	}
}
