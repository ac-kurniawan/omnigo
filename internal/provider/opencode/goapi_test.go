package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestChatCompletionsDropsUnsafeUpstreamHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=evil")
		w.Header().Set("X-Internal-Debug", "secret-topology")
		w.Write([]byte(`{"id":"c","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer server.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "k"}}
	p := New(provider.Config{Name: "oc", BaseURL: server.URL}, store).(*Provider)
	rec := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "glm-5.3-flash"}, rec); err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie forwarded: %q", got)
	}
	if got := rec.Header().Get("X-Internal-Debug"); got != "" {
		t.Fatalf("debug header forwarded: %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got == "" {
		t.Fatal("Content-Type must be forwarded")
	}
}

func TestParseFlatArrayShape(t *testing.T) {
	body := `[{"id":"glm-5","name":"GLM-5"}]`
	models, protos, err := parseModelsWithProtocols(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "glm-5" {
		t.Fatalf("expected 1 model glm-5, got %+v", models)
	}
	if protos["glm-5"] != protocolChat {
		t.Fatalf("glm-5: expected chat, got %v", protos["glm-5"])
	}
}

func TestParseLiveModelsShape(t *testing.T) {
	body := `{"object":"list","data":[
		{"id":"qwen3.8-max","name":"Qwen3.8 Max","capabilities":{"reasoning":{"effort":["low","medium","xhigh"]}},"api":{"messages":{"base_url":"https://opencode.ai/inference/go/anthropic/v1"}}},
		{"id":"grok-4.7","name":"Grok 4.7","capabilities":{"reasoning":{"effort":["low","medium"]}},"api":{"responses":{"base_url":"https://opencode.ai/inference/go/openai/v1"}}},
		{"id":"glm-5.3-flash","name":"GLM","api":{"chat-completions":{"base_url":"https://opencode.ai/inference/go/openai/v1"}}}
	]}`
	models, protos, err := parseModelsWithProtocols(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	if protos["qwen3.8-max"] != protocolMessages {
		t.Fatalf("qwen3.8-max: expected messages, got %v", protos["qwen3.8-max"])
	}
	if protos["grok-4.7"] != protocolResponses {
		t.Fatalf("grok-4.7: expected responses, got %v", protos["grok-4.7"])
	}
	if protos["glm-5.3-flash"] != protocolChat {
		t.Fatalf("glm-5.3-flash: expected chat, got %v", protos["glm-5.3-flash"])
	}
	if models[0].Capabilities == nil || len(models[0].Capabilities.ReasoningEfforts) != 3 {
		t.Fatalf("expected array effort parsed, got %+v", models[0].Capabilities)
	}
}

func TestClassifyModelExtended(t *testing.T) {
	if got := classifyModel("qwen3.8-max"); got != protocolMessages {
		t.Fatalf("qwen3.8-max: expected messages, got %v", got)
	}
	if got := classifyModel("minimax-m3"); got != protocolMessages {
		t.Fatalf("minimax-m3: expected messages, got %v", got)
	}
	if got := classifyModel("gemini-4-argon"); got != protocolChat {
		t.Fatalf("gemini-4-argon: expected chat, got %v", got)
	}
}

func TestGoAPIModelsPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	// Emulate Go API base: provider must request /v1/models under the go root.
	store := &memoryCredStore{creds: provider.Credentials{APIKey: "k"}}
	p := New(provider.Config{Name: "oc", BaseURL: "https://opencode.ai/inference/go"}, store).(*Provider)
	p.client = server.Client()
	// Rewrite baseURL to test server while keeping go-routing marker.
	p.baseURL = server.URL + "/inference/go"
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/inference/go/v1/models" {
		t.Fatalf("expected /inference/go/v1/models, got %s", gotPath)
	}
}

func TestGoAPIChatPaths(t *testing.T) {
	cases := []struct {
		model string
		path  string
	}{
		{"glm-5.3-flash", "/inference/go/openai/v1/chat/completions"},
		{"gpt-5.6-luna", "/inference/go/openai/v1/responses"},
		{"qwen3.8-max", "/inference/go/anthropic/v1/messages"},
	}
	for _, tc := range cases {
		var gotPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "text/event-stream")
			// Minimal SSE so the translators can finish.
			if strings.HasSuffix(r.URL.Path, "/responses") {
				w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
				w.Write([]byte("data: [DONE]\n\n"))
				return
			}
			if strings.HasSuffix(r.URL.Path, "/messages") {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"m","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"c","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		}))
		store := &memoryCredStore{creds: provider.Credentials{APIKey: "k"}}
		p := New(provider.Config{Name: "oc", BaseURL: server.URL + "/inference/go"}, store).(*Provider)
		rec := httptest.NewRecorder()
		if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: tc.model}, rec); err != nil {
			server.Close()
			t.Fatalf("%s: %v", tc.model, err)
		}
		server.Close()
		if gotPath != tc.path {
			t.Fatalf("%s: expected %s, got %s", tc.model, tc.path, gotPath)
		}
	}
}
