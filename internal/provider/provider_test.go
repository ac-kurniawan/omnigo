package provider

import (
	"encoding/json"
	"sync"
	"testing"
)

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
