package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

type memoryCredStore struct {
	creds provider.Credentials
}

func (s *memoryCredStore) Get() provider.Credentials { return s.creds }
func (s *memoryCredStore) Put(c provider.Credentials) error {
	s.creds = c
	return nil
}

func TestRegistration(t *testing.T) {
	factory, ok := provider.Get("opencode")
	if !ok {
		t.Fatal("expected opencode to be registered in provider registry")
	}
	p := factory(provider.Config{Name: "opencode-test"}, &memoryCredStore{})
	if p == nil {
		t.Fatal("expected non-nil provider from factory")
	}
	if p.Name() != "opencode-test" {
		t.Fatalf("expected name opencode-test, got %s", p.Name())
	}
}

func TestDefaultAndCustomBaseURL(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{APIKey: "oc_sk_123"}}
	pDef := New(provider.Config{Name: "opencode"}, store).(*Provider)
	if pDef.baseURL != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("expected default baseURL https://opencode.ai/zen/go/v1, got %s", pDef.baseURL)
	}

	pCustom := New(provider.Config{Name: "opencode", BaseURL: "https://opencode.ai/inference/go/"}, store).(*Provider)
	if pCustom.baseURL != "https://opencode.ai/inference/go" {
		t.Fatalf("expected trimmed baseURL https://opencode.ai/inference/go, got %s", pCustom.baseURL)
	}
}

func TestClassifyModel(t *testing.T) {
	tests := []struct {
		model    string
		expected protocol
	}{
		{"claude-haiku-5-5", protocolMessages},
		{"claude-3-5-sonnet", protocolMessages},
		{"gpt-5.6-luna", protocolResponses},
		{"gpt-6-luna", protocolResponses},
		{"grok-3", protocolResponses},
		{"muse-spark-v1", protocolResponses},
		{"deepseek-v4.1-flash", protocolChat},
		{"glm-5", protocolChat},
		{"kimi-k2", protocolChat},
		{"minimax-text-01", protocolChat},
		{"qwen-2.5-72b", protocolMessages},
		{"unknown-model", protocolChat},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := classifyModel(tt.model)
			if got != tt.expected {
				t.Fatalf("model %s: expected %v, got %v", tt.model, tt.expected, got)
			}
		})
	}
}

func TestStableSessionID(t *testing.T) {
	// Test with explicit session_id
	reqWithSession := provider.ChatRequest{
		Raw: []byte(`{"session_id":"my-session-123"}`),
	}
	id1 := stableSessionID(context.Background(), reqWithSession)
	if !strings.HasPrefix(id1, "ses_") || len(id1) != 36 {
		t.Fatalf("expected ses_<32hex>, got %s", id1)
	}

	// Test with caller context
	caller := &auth.Caller{}
	caller.Set(vault.ClientKey{ID: "user-456"})
	ctx := auth.WithCaller(context.Background(), caller)
	id2 := stableSessionID(ctx, provider.ChatRequest{})
	if !strings.HasPrefix(id2, "ses_") {
		t.Fatalf("expected ses_ prefix, got %s", id2)
	}

	// Test with raw key
	reqWithKey := provider.ChatRequest{
		Raw: []byte(`{"prompt_cache_key":"pc_789"}`),
	}
	id3 := stableSessionID(context.Background(), reqWithKey)
	if id3 == id2 {
		t.Fatalf("stable session id should be deterministic")
	}
}

func TestChatCompletionsDispatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "wrong path", 404)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "missing auth", 401)
			return
		}
		if r.Header.Get("User-Agent") != customUserAgent {
			http.Error(w, "missing UA", 400)
			return
		}
		if r.Header.Get(sessionHeader) == "" {
			http.Error(w, "missing session header", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"cmpl-123","object":"chat.completion","choices":[{"delta":{"content":"hi"}}]}`))
	}))
	defer server.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "oc_sk_123"}}
	p := New(provider.Config{Name: "opencode", BaseURL: server.URL}, store).(*Provider)

	req := provider.ChatRequest{
		Model:    "deepseek-v4.1-flash",
		Stream:   false,
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}
	rec := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatal(err)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("expected non-empty response body")
	}
}

func TestMessagesDispatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			http.Error(w, "wrong path", 404)
			return
		}
		if r.Header.Get("x-api-key") == "" {
			http.Error(w, "missing x-api-key", 401)
			return
		}
		if r.Header.Get("anthropic-version") != anthropicVersion {
			http.Error(w, "missing anthropic-version", 400)
			return
		}
		if r.Header.Get(sessionHeader) == "" {
			http.Error(w, "missing session header", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_123","content":[{"type":"text","text":"hi"}]}`))
	}))
	defer server.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "oc_sk_123"}}
	p := New(provider.Config{Name: "opencode", BaseURL: server.URL}, store).(*Provider)

	req := provider.ChatRequest{
		Model:    "claude-haiku-5-5",
		Stream:   false,
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}
	rec := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatal(err)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("expected non-empty response body")
	}
}

func TestResponsesDispatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.Error(w, "wrong path", 404)
			return
		}
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "missing auth", 401)
			return
		}
		if r.Header.Get(sessionHeader) == "" {
			http.Error(w, "missing session header", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"response.created\"}\n\n"))
		w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
		w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "oc_sk_123"}}
	p := New(provider.Config{Name: "opencode", BaseURL: server.URL}, store).(*Provider)

	req := provider.ChatRequest{
		Model:    "gpt-5.6-luna",
		Stream:   true,
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}
	rec := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatal(err)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("expected non-empty response body")
	}
}

func TestModelsEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.Error(w, "wrong path", 404)
			return
		}
		if r.Header.Get("User-Agent") != customUserAgent {
			http.Error(w, "wrong user-agent", 400)
			return
		}
		if r.Header.Get("Authorization") != "Bearer oc_sk_123" {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"data": [
				{
					"id": "deepseek-v4.1-flash",
					"name": "DeepSeek V4.1 Flash",
					"capabilities": {
						"reasoning": {
							"effort": {
								"efforts": ["low", "medium", "high"],
								"default": "medium"
							}
						}
					}
				},
				{
					"id": "glm-5"
				}
			]
		}`))
	}))
	defer server.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "oc_sk_123"}}
	p := New(provider.Config{Name: "opencode", BaseURL: server.URL}, store).(*Provider)

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "deepseek-v4.1-flash" || models[0].Name != "DeepSeek V4.1 Flash" {
		t.Fatalf("unexpected model[0]: %+v", models[0])
	}
	if models[0].Capabilities == nil || !models[0].Capabilities.Reasoning {
		t.Fatalf("expected reasoning capabilities on model 0, got %+v", models[0].Capabilities)
	}
	if len(models[0].Capabilities.ReasoningEfforts) != 3 || models[0].Capabilities.DefaultEffort != "medium" {
		t.Fatalf("unexpected efforts: %+v", models[0].Capabilities)
	}
	if models[1].ID != "glm-5" || models[1].Name != "glm-5" || models[1].Capabilities != nil {
		t.Fatalf("unexpected model[1]: %+v", models[1])
	}
}

func TestTestEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[]}`))
			return
		}
		http.Error(w, "unexpected", 404)
	}))
	defer server.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "oc_sk_123"}}
	p := New(provider.Config{Name: "opencode", BaseURL: server.URL}, store).(*Provider)

	res := p.Test(context.Background())
	if !res.OK {
		t.Fatalf("expected Test OK, got %v", res)
	}

	// Test failure case
	serverFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer serverFail.Close()

	pFail := New(provider.Config{Name: "opencode", BaseURL: serverFail.URL}, store).(*Provider)
	resFail := pFail.Test(context.Background())
	if resFail.OK || resFail.Error == "" {
		t.Fatalf("expected Test failure, got %+v", resFail)
	}
}

func TestUsageReportingAndErrorSanitization(t *testing.T) {
	t.Run("ChatCompletions Usage", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"cmpl-1","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`))
		}))
		defer server.Close()

		store := &memoryCredStore{creds: provider.Credentials{APIKey: "oc_sk_123", AccountID: "acc-1"}}
		p := New(provider.Config{Name: "opencode", BaseURL: server.URL}, store).(*Provider)

		var captured provider.TokenUsage
		ctx := provider.WithUsageSink(context.Background(), func(u provider.TokenUsage) {
			captured = u
		})

		rec := httptest.NewRecorder()
		err := p.ChatCompletion(ctx, provider.ChatRequest{Model: "deepseek-v4.1-flash"}, rec)
		if err != nil {
			t.Fatal(err)
		}
		if !captured.Present || captured.InputTokens != 10 || captured.OutputTokens != 20 {
			t.Fatalf("unexpected captured usage: %+v", captured)
		}
		if captured.Account != "acc-1" {
			t.Fatalf("expected account acc-1, got %q", captured.Account)
		}
	})

	t.Run("Error Sanitization", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "sensitive internal leak with secret oc_sk_secret_token", http.StatusBadRequest)
		}))
		defer server.Close()

		store := &memoryCredStore{creds: provider.Credentials{APIKey: "oc_sk_123"}}
		p := New(provider.Config{Name: "opencode", BaseURL: server.URL}, store).(*Provider)

		rec := httptest.NewRecorder()
		err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "deepseek-v4.1-flash"}, rec)
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), "oc_sk_secret_token") {
			t.Fatalf("error contains raw upstream secret: %v", err)
		}
		if !strings.Contains(err.Error(), "opencode: upstream status 400") {
			t.Fatalf("expected sanitized status error, got: %v", err)
		}
	})
}
