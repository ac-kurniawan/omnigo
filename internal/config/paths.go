package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultConfigYAML is the initial starter configuration seeded when no config.yaml exists.
const DefaultConfigYAML = `server:
  host: 0.0.0.0
  port: 8080
  timeout: 20s
  stream_timeout: 10m

providers:
  - name: agy
    type: antigravity
    base_url: https://cloudcode-pa.googleapis.com
    models:
      - gemini-3.7-flash-medium
      - claude-sonnet-4-6

combos:
  - name: auto
    strategy: priority
    targets:
      - provider: agy
        model: gemini-3.7-flash-medium
`

type Paths struct {
	Dir    string
	Config string
	Auth   string
	Key    string
	Drains string
}

// DefaultConfigDir returns the default configuration directory (~/.config/omnigo on unix).
func DefaultConfigDir() string {
	userDir, err := os.UserConfigDir()
	if err != nil || userDir == "" {
		home := os.Getenv("HOME")
		if home == "" {
			home = "."
		}
		userDir = filepath.Join(home, ".config")
	}
	return filepath.Join(userDir, "omnigo")
}

// ResolvePaths determines the configuration, auth, and key file paths,
// ensures the target directory exists, and seeds a default config.yaml if missing.
func ResolvePaths(dirFlag, cfgFlag, authFlag, keyFlag string) (*Paths, error) {
	dir := dirFlag
	if dir == "" {
		dir = os.Getenv("OMNIGO_CONFIG_DIR")
		if dir == "" {
			dir = DefaultConfigDir()
		}
	}

	cfgPath := cfgFlag
	if cfgPath == "" {
		cfgPath = filepath.Join(dir, "config.yaml")
	}

	authPath := authFlag
	if authPath == "" {
		authPath = filepath.Join(dir, "auth.yaml")
	}

	keyPath := keyFlag
	if keyPath == "" {
		keyPath = filepath.Join(dir, ".secret.key")
	}

	drainsPath := filepath.Join(dir, "drains.json")

	// Ensure the base directory or config's parent directory exists
	targetDir := dir
	if cfgFlag != "" {
		targetDir = filepath.Dir(cfgPath)
	}
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		return nil, fmt.Errorf("create config directory %s: %w", targetDir, err)
	}

	// Also ensure auth and key parent directories exist if overridden
	if authDir := filepath.Dir(authPath); authDir != targetDir {
		_ = os.MkdirAll(authDir, 0700)
	}
	if keyDir := filepath.Dir(keyPath); keyDir != targetDir {
		_ = os.MkdirAll(keyDir, 0700)
	}

	// Check if config.yaml exists; if not, initialize it
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		// If a local config.example.yaml exists, copy from it, else use DefaultConfigYAML
		content := []byte(DefaultConfigYAML)
		if exampleBytes, err := os.ReadFile("config.example.yaml"); err == nil && len(exampleBytes) > 0 {
			content = exampleBytes
		}
		if err := os.WriteFile(cfgPath, content, 0644); err != nil {
			return nil, fmt.Errorf("seed initial config to %s: %w", cfgPath, err)
		}
	}

	return &Paths{
		Dir:    dir,
		Config: cfgPath,
		Auth:   authPath,
		Key:    keyPath,
		Drains: drainsPath,
	}, nil
}
