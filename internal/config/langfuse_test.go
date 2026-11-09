package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLangfuseDefaults(t *testing.T) {
	cfg, err := loadYAML(t, "providers: []\ncombos: []\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	l := cfg.Observability.Langfuse
	if l.EnabledOrDefault() {
		t.Error("expected disabled by default")
	}
	if l.CaptureContentEnabled() {
		t.Error("expected capture_content off by default")
	}
	if got := l.HostOrDefault(); got != "https://cloud.langfuse.com" {
		t.Errorf("host = %q, want https://cloud.langfuse.com", got)
	}
}

func TestLangfuseHostTrimsTrailingSlash(t *testing.T) {
	l := Langfuse{Host: "https://lf.example.com///"}
	if got := l.HostOrDefault(); got != "https://lf.example.com" {
		t.Errorf("host = %q", got)
	}
}

func TestLangfuseEnabledRejectsBadHost(t *testing.T) {
	_, err := loadYAML(t, `observability:
  langfuse:
    enabled: true
    host: ftp://x
providers: []
combos: []
`)
	if err == nil {
		t.Fatal("expected error for ftp host when enabled")
	}
	if !strings.Contains(err.Error(), "observability.langfuse: invalid host") {
		t.Errorf("err = %v", err)
	}
}

func TestLangfuseDisabledIgnoresBadHost(t *testing.T) {
	if _, err := loadYAML(t, `observability:
  langfuse:
    enabled: false
    host: ftp://x
providers: []
combos: []
`); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLangfuseEnabledAcceptsHTTPHost(t *testing.T) {
	cfg, err := loadYAML(t, `observability:
  langfuse:
    enabled: true
    host: http://127.0.0.1:9999
    capture_content: true
providers: []
combos: []
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Observability.Langfuse.CaptureContentEnabled() {
		t.Error("expected capture_content on")
	}
}
