package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePathsWithExplicitDir(t *testing.T) {
	tempDir := t.TempDir()
	customDir := filepath.Join(tempDir, "custom-omnigo")

	paths, err := ResolvePaths(customDir, "", "", "")
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}

	if paths.Dir != customDir {
		t.Errorf("Dir = %q, want %q", paths.Dir, customDir)
	}
	wantCfg := filepath.Join(customDir, "config.yaml")
	if paths.Config != wantCfg {
		t.Errorf("Config = %q, want %q", paths.Config, wantCfg)
	}
	wantAuth := filepath.Join(customDir, "auth.yaml")
	if paths.Auth != wantAuth {
		t.Errorf("Auth = %q, want %q", paths.Auth, wantAuth)
	}
	wantKey := filepath.Join(customDir, ".secret.key")
	if paths.Key != wantKey {
		t.Errorf("Key = %q, want %q", paths.Key, wantKey)
	}

	// Verify directory was created
	fi, err := os.Stat(customDir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("directory was not created: %v", err)
	}

	// Verify default config.yaml was auto-seeded
	if _, err := os.Stat(wantCfg); err != nil {
		t.Fatalf("config.yaml was not seeded: %v", err)
	}

	// Verify the seeded config loads cleanly
	loaded, err := Load(wantCfg)
	if err != nil {
		t.Fatalf("Load seeded config failed: %v", err)
	}
	if loaded.Server.Port != 8080 {
		t.Errorf("seeded port = %d, want 8080", loaded.Server.Port)
	}
}

func TestResolvePathsPreservesExistingConfig(t *testing.T) {
	tempDir := t.TempDir()
	customDir := filepath.Join(tempDir, "existing-dir")
	if err := os.MkdirAll(customDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(customDir, "config.yaml")
	existingContent := "server:\n  host: 127.0.0.1\n  port: 9999\n"
	if err := os.WriteFile(cfgFile, []byte(existingContent), 0644); err != nil {
		t.Fatal(err)
	}

	paths, err := ResolvePaths(customDir, "", "", "")
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}

	loaded, err := Load(paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Server.Port != 9999 {
		t.Fatalf("expected existing config port 9999, got %d", loaded.Server.Port)
	}
}

func TestResolvePathsExplicitOverrides(t *testing.T) {
	tempDir := t.TempDir()
	explicitCfg := filepath.Join(tempDir, "my-conf.yaml")
	explicitAuth := filepath.Join(tempDir, "my-auth.yaml")
	explicitKey := filepath.Join(tempDir, "my-key.key")

	paths, err := ResolvePaths("", explicitCfg, explicitAuth, explicitKey)
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}

	if paths.Config != explicitCfg {
		t.Errorf("Config = %q, want %q", paths.Config, explicitCfg)
	}
	if paths.Auth != explicitAuth {
		t.Errorf("Auth = %q, want %q", paths.Auth, explicitAuth)
	}
	if paths.Key != explicitKey {
		t.Errorf("Key = %q, want %q", paths.Key, explicitKey)
	}
}

func TestResolvePathsEnvDir(t *testing.T) {
	tempDir := t.TempDir()
	envDir := filepath.Join(tempDir, "env-omnigo")
	t.Setenv("OMNIGO_CONFIG_DIR", envDir)

	paths, err := ResolvePaths("", "", "", "")
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}

	if paths.Dir != envDir {
		t.Errorf("Dir = %q, want %q", paths.Dir, envDir)
	}
	if _, err := os.Stat(paths.Config); err != nil {
		t.Fatalf("expected config seeded in env dir: %v", err)
	}
}
