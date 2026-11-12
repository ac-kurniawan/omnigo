package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// LoadEnvFile reads KEY=VALUE lines from path and sets any variable that is
// not already in the environment, so a real environment always wins over the
// file. A missing file is a no-op, which keeps -dir and container deploys
// working without one.
//
// It is deliberately a small subset of dotenv: comments, blank lines,
// `export ` prefixes, and single or double quoted values. Nothing else
// (interpolation, multiline values) is supported.
func LoadEnvFile(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := parseEnvLine(line)
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}
	return nil
}

func parseEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", false
	}
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		quote := value[0]
		if (quote == '"' || quote == '\'') && value[len(value)-1] == quote {
			return key, value[1 : len(value)-1], true
		}
	}
	// An unquoted value ends at the first " #": a bare # inside a token
	// (a password, a URL fragment) is kept.
	if i := strings.Index(value, " #"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return key, value, true
}
