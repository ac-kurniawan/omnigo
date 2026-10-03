package provider

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
)

func TestModelCapabilitiesJSON(t *testing.T) {
	for _, tc := range []struct {
		model Model
		want  string
	}{
		{Model{ID: "reasoning", Capabilities: &ModelCapabilities{Reasoning: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "medium"}}, `{"id":"reasoning","name":"","capabilities":{"reasoning":true,"reasoning_efforts":["low","medium","high"],"default_effort":"medium"}}`},
		{Model{ID: "plain"}, `{"id":"plain","name":""}`},
	} {
		got, err := json.Marshal(tc.model)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Fatalf("model JSON = %s, want %s", got, tc.want)
		}
	}
}

func TestCapabilitiesFor(t *testing.T) {
	want := &ModelCapabilities{Reasoning: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "medium"}
	hits := []struct{ typ, id string }{
		{"openai", "o1"},
		{"openai", "o3-mini"},
		{"openai", "O4-MINI"},
		{"openai", "xai/grok-4.7"},
		{"openai", "deepseek-v4.1-flash"},
		{"openai", "deepseek-r1"},
		{"openai", "qwen-thinking"},
		{"openai", "model-thinking-latest"},
		{"codex", "o1"},
		{"antigravity", "gemini-3.7-flash-high"},
		{"antigravity", "gemini-3.7-flash-medium"},
		{"antigravity", "gemini-3.7-flash-low"},
		{"antigravity", "gemini-3.1-pro-low"},
		{"antigravity", "claude-opus-4-6-thinking"},
		{"antigravity", "gpt-oss-120b-medium"},
		{"antigravity", "model-extra-low"},
		{"antigravity", "model-tiered"},
		{"antigravity", "model-high-preview"},
	}
	for _, tc := range hits {
		got := CapabilitiesFor(tc.typ, tc.id)
		if got == nil || got.Reasoning != want.Reasoning || got.DefaultEffort != want.DefaultEffort || !reflect.DeepEqual(got.ReasoningEfforts, want.ReasoningEfforts) {
			t.Fatalf("CapabilitiesFor(%q, %q) = %+v, want %+v", tc.typ, tc.id, got, want)
		}
	}
	misses := []struct{ typ, id string }{
		{"openai", "gpt-4o"},
		{"openai", "gpt-4o-mini"},
		{"openai", "openai-gpt-4o"},
		{"openai", "claude-sonnet-4-6"},
		{"openai", "gemini-3.1-flash-lite"},
		{"codex", "gpt-5.2"},
		{"antigravity", "claude-sonnet-4-6"},
		{"antigravity", "gemini-3.1-flash-lite"},
		{"antigravity", "gpt-4o"},
		{"", "o1"},
		{"other", "o1"},
	}
	for _, tc := range misses {
		if got := CapabilitiesFor(tc.typ, tc.id); got != nil {
			t.Fatalf("CapabilitiesFor(%q, %q) = %+v, want nil", tc.typ, tc.id, got)
		}
	}
	a := CapabilitiesFor("openai", "o1")
	b := CapabilitiesFor("openai", "o1")
	a.ReasoningEfforts[0] = "mutated"
	if b.ReasoningEfforts[0] != "low" {
		t.Fatal("CapabilitiesFor returned a shared efforts slice")
	}
}

func TestChatRequestBodyReplacesModel(t *testing.T) {
	req := ChatRequest{
		Model: "routers9/deepseek-v4-flash-0731",
		Raw:   []byte(`{"model":"myrouter/routers9/deepseek-v4-flash-0731","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`),
	}
	body, err := req.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got["model"] != "routers9/deepseek-v4-flash-0731" {
		t.Fatalf("model = %v, want bare resolved id", got["model"])
	}
	if got["temperature"] != 0.7 {
		t.Fatalf("temperature = %v, want preserved", got["temperature"])
	}
	if _, ok := got["messages"]; !ok {
		t.Fatal("messages field dropped")
	}
}

func TestChatRequestBodySynthesizesWithoutRaw(t *testing.T) {
	req := ChatRequest{Model: "gpt-4o", Stream: true, Messages: []Message{{Role: "user", Content: "hi"}}}
	body, err := req.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	var got struct {
		Model    string    `json:"model"`
		Stream   bool      `json:"stream"`
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got.Model != "gpt-4o" || !got.Stream || len(got.Messages) != 1 {
		t.Fatalf("body = %+v", got)
	}
}

func TestChatRequestBodyNormalizesDeveloperRole(t *testing.T) {
	req := ChatRequest{
		Model: "gemini-3.8-flash",
		Raw:   []byte(`{"model":"myrouter/gemini-3.8-flash","messages":[{"role":"developer","content":"system instructions"},{"role":"user","content":"hi"}]}`),
	}
	body, err := req.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" {
		t.Fatalf("expected developer role normalized to system, got %+v", got.Messages)
	}

	if got.Messages[1].Role != "user" {
		t.Fatalf("expected user role preserved, got %+v", got.Messages[1])
	}
}

func TestChatRequestBodyReusesParsedPayloadAndCachesEncodedBytes(t *testing.T) {
	raw := []byte(`{"model":"auto","messages":[{"role":"developer","content":"system instructions"},{"role":"user","content":"hi"}],"temperature":0.7}`)
	parsed := ParseBody(raw)
	if parsed == nil {
		t.Fatal("ParseBody returned nil for valid request")
	}
	req := ChatRequest{Model: "gpt-a", Raw: raw, Parsed: parsed}

	first, err := req.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	second, err := req.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if &first[0] != &second[0] {
		t.Fatal("Body re-encoded instead of reusing cached bytes")
	}

	req.Model = "gpt-b"
	other, err := req.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if &first[0] == &other[0] {
		t.Fatal("Body reused bytes across models")
	}

	var got map[string]any
	if err := json.Unmarshal(other, &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got["model"] != "gpt-b" || got["temperature"] != 0.7 {
		t.Fatalf("body = %#v", got)
	}
	messages := got["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("developer role not normalized: %#v", messages)
	}

	// The shared parsed payload stays untouched for non-OpenAI translators.
	if parsed["model"] != "auto" {
		t.Fatalf("shared payload model mutated to %v", parsed["model"])
	}
	messages = parsed["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "developer" {
		t.Fatalf("shared payload role mutated: %#v", messages)
	}
}

func TestChatRequestBodyConcurrentCallsAreRaceFree(t *testing.T) {
	raw := []byte(`{"model":"auto","messages":[{"role":"developer","content":"system instructions"},{"role":"user","content":"hi"}],"temperature":0.2}`)
	parsed := ParseBody(raw)
	if parsed == nil {
		t.Fatal("ParseBody returned nil for valid request")
	}

	const goroutines = 32
	models := []string{"gpt-a", "gpt-b"}
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*len(models))
	bodies := make([][][]byte, len(models))
	for i := range bodies {
		bodies[i] = make([][]byte, goroutines)
	}

	for m, model := range models {
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(m, g int, model string) {
				defer wg.Done()
				req := ChatRequest{Model: model, Raw: raw, Parsed: parsed}
				body, err := req.Body()
				if err != nil {
					errCh <- err
					return
				}
				bodies[m][g] = body
			}(m, g, model)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("Body: %v", err)
	}

	for m, model := range models {
		var first []byte
		for g := 0; g < goroutines; g++ {
			body := bodies[m][g]
			if len(body) == 0 {
				t.Fatalf("model %s goroutine %d returned empty body", model, g)
			}
			if first == nil {
				first = body
				continue
			}
			if &body[0] != &first[0] {
				t.Fatalf("model %s did not share encoded bytes", model)
			}
			if string(body) != string(first) {
				t.Fatalf("model %s encoded %q, want %q", model, body, first)
			}
		}
		var got struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(first, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", model, err)
		}
		if got.Model != model {
			t.Fatalf("model = %q, want %q", got.Model, model)
		}
		if len(got.Messages) != 2 || got.Messages[0].Role != "system" {
			t.Fatalf("developer role not normalized for %s: %+v", model, got.Messages)
		}
	}
	if &bodies[0][0][0] == &bodies[1][0][0] {
		t.Fatal("different models shared encoded bytes")
	}
}
func TestHTTPStatusErrorDrainability(t *testing.T) {
	err400 := NewHTTPStatusError(400, "bad request")
	if err400.Drainable() {
		t.Fatal("400 Bad Request should not be drainable")
	}
	err429 := NewHTTPStatusError(429, "rate limited")
	if !err429.Drainable() {
		t.Fatal("429 Too Many Requests should be drainable")
	}
	err500 := NewHTTPStatusError(500, "internal error")
	if !err500.Drainable() {
		t.Fatal("500 Internal Error should be drainable")
	}
}
