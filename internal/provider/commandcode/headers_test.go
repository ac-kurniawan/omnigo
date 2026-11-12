package commandcode

import (
	"net/http"
	"strings"
	"testing"
)

func TestHeadersExactSet(t *testing.T) {
	h := buildCommandCodeHeaders("k", "sess-1", "text/event-stream")
	checks := map[string]string{
		"Content-Type":           "application/json",
		"Authorization":          "Bearer k",
		"Accept":                 "text/event-stream",
		"User-Agent":             "cli",
		"x-command-code-version": CommandCodeCLIVersion,
		"x-cli-environment":      "production",
		"x-cli-surface":          "cli",
		"x-taste-learning":       "false",
		"x-session-id":           "sess-1",
	}
	for key, want := range checks {
		if got := h.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if got := h.Get("x-project-slug"); got == "" || strings.Contains(got, " ") {
		t.Fatalf("x-project-slug = %q", got)
	}
	if CommandCodeCLIVersion != "1.88.5" {
		t.Fatalf("version = %q", CommandCodeCLIVersion)
	}
	var _ http.Header = h
}

func TestSlugCases(t *testing.T) {
	cases := map[string]string{
		"omnigo":     "omnigo",
		"MyProject":  "my-project",
		"my_project": "my-project",
		"  spaced  ": "spaced",
		"a--b":       "a-b",
		"München":    "muenchen",
		"":           "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestThreadIDShapeAndSlugFallback(t *testing.T) {
	id := newThreadID()
	parts := strings.Split(id, "-")
	if len(parts) != 5 || len(id) != 36 {
		t.Fatalf("thread id shape = %q", id)
	}
}
