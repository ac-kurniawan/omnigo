package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `# comment line
OMNIGO_TEST_PLAIN=hello
OMNIGO_TEST_QUOTED="quoted value"
OMNIGO_TEST_SINGLE='single value'
OMNIGO_TEST_EXPORT=exported
export OMNIGO_TEST_EXPORTED=yes
OMNIGO_TEST_HASH=value # trailing comment
OMNIGO_TEST_EMPTY=
  OMNIGO_TEST_INDENTED = spaced  
OMNIGO_TEST_WITH_EQUALS=a=b=c
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// A variable already in the environment must win over the file.
	t.Setenv("OMNIGO_TEST_EXPORT", "from-environment")

	for _, key := range []string{
		"OMNIGO_TEST_PLAIN", "OMNIGO_TEST_QUOTED", "OMNIGO_TEST_SINGLE",
		"OMNIGO_TEST_EXPORTED", "OMNIGO_TEST_HASH", "OMNIGO_TEST_EMPTY",
		"OMNIGO_TEST_INDENTED", "OMNIGO_TEST_WITH_EQUALS",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}

	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}

	cases := map[string]string{
		"OMNIGO_TEST_PLAIN":       "hello",
		"OMNIGO_TEST_QUOTED":      "quoted value",
		"OMNIGO_TEST_SINGLE":      "single value",
		"OMNIGO_TEST_EXPORT":      "from-environment",
		"OMNIGO_TEST_EXPORTED":    "yes",
		"OMNIGO_TEST_HASH":        "value",
		"OMNIGO_TEST_EMPTY":       "",
		"OMNIGO_TEST_INDENTED":    "spaced",
		"OMNIGO_TEST_WITH_EQUALS": "a=b=c",
	}
	for key, want := range cases {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestLoadEnvFileMissingFileIsNotAnError(t *testing.T) {
	if err := LoadEnvFile(filepath.Join(t.TempDir(), "absent.env")); err != nil {
		t.Fatalf("missing file must be a no-op, got %v", err)
	}
}

func TestLoadEnvFileIgnoresMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("no-equals-here\n=novalue\nOMNIGO_TEST_OK=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMNIGO_TEST_OK", "")
	os.Unsetenv("OMNIGO_TEST_OK")

	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if got := os.Getenv("OMNIGO_TEST_OK"); got != "1" {
		t.Fatalf("OMNIGO_TEST_OK = %q, want 1", got)
	}
}

func TestLoadEnvFileCRLF(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("OMNIGO_TEST_CRLF=one\r\nOMNIGO_TEST_CRLF2=two\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"OMNIGO_TEST_CRLF", "OMNIGO_TEST_CRLF2"} {
		os.Unsetenv(key)
	}
	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	for key, want := range map[string]string{"OMNIGO_TEST_CRLF": "one", "OMNIGO_TEST_CRLF2": "two"} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want %q (stray CR not stripped)", key, got, want)
		}
	}
}

func TestLoadEnvFileDuplicateKeyKeepsFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("OMNIGO_TEST_DUP=first\nOMNIGO_TEST_DUP=second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("OMNIGO_TEST_DUP")
	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if got := os.Getenv("OMNIGO_TEST_DUP"); got != "first" {
		t.Fatalf("duplicate key kept %q, want first", got)
	}
}
