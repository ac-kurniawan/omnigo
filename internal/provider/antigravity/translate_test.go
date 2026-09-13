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

func TestToEnvelopeExtractsMultimodalTextParts(t *testing.T) {
	req := provider.ChatRequest{
		Model: "gemini-3.7-flash-medium",
		Messages: []provider.Message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "describe this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/image.png"}},
				map[string]any{"type": "text", "text": "please"},
			},
		}},
	}
	env, err := ToEnvelope("proj-1", req.Model, req)
	if err != nil {
		t.Fatalf("ToEnvelope: %v", err)
	}
	contents := env["request"].(map[string]any)["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["text"] != "describe this" || parts[1].(map[string]any)["text"] != "please" {
		t.Fatalf("parts = %#v", parts)
	}
}

func TestToEnvelopeIgnoresImageOnlyMessage(t *testing.T) {
	req := provider.ChatRequest{
		Model: "gemini-3.7-flash-medium",
		Messages: []provider.Message{
			{Role: "user", Content: []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
			}},
			{Role: "user", Content: "follow-up text"},
		},
	}
	env, err := ToEnvelope("proj-1", req.Model, req)
	if err != nil {
		t.Fatalf("ToEnvelope rejected image-only message: %v", err)
	}
	contents := env["request"].(map[string]any)["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents = %v, want image-only message skipped", contents)
	}
	parts := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "follow-up text" {
		t.Fatalf("parts = %v, want follow-up text", parts)
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
