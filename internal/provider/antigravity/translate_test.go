package antigravity

import (
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestToEnvelopeMapsRolesAndParts(t *testing.T) {
	req := provider.ChatRequest{
		Model: "gemini-3.7-flash-medium",
		Messages: []provider.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		},
	}
	env, err := ToEnvelope("proj-1", req.Model, req)
	if err != nil {
		t.Fatalf("ToEnvelope: %v", err)
	}
	if env["project"] != "proj-1" {
		t.Fatalf("project = %v", env["project"])
	}
	if env["model"] != "gemini-3.7-flash-medium" {
		t.Fatalf("model = %v", env["model"])
	}
	reqInner, ok := env["request"].(map[string]any)
	if !ok {
		t.Fatalf("no request envelope: %v", env)
	}
	contents, ok := reqInner["contents"].([]any)
	if !ok || len(contents) != 2 {
		t.Fatalf("contents = %v", reqInner["contents"])
	}
	first := contents[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("role = %v", first["role"])
	}
	parts := first["parts"].([]any)
	if parts[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("parts = %v", parts)
	}
}

func TestTranslateSSEDelta(t *testing.T) {
	gemini := []byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hel"}]}}]}`)
	out, err := TranslateSSE(gemini)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"delta":{"content":"hel"}`) {
		t.Fatalf("out = %s", out)
	}
	if !strings.HasPrefix(string(out), "data: ") {
		t.Fatalf("out = %s", out)
	}
}

func TestTranslateSSESkipsEmptyCandidate(t *testing.T) {
	gemini := []byte(`data: {"candidates":[]}`)
	out, err := TranslateSSE(gemini)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("expected nil for metadata chunk, got %s", out)
	}
}
