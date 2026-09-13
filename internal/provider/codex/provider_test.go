package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func event(typ string, fields map[string]any) string {
	fields["type"] = typ
	b, _ := json.Marshal(fields)
	return "event: " + typ + "\ndata: " + string(b) + "\n\n"
}

func codexTestStream() string {
	return event("response.created", map[string]any{"response": map[string]any{"id": "resp_1", "model": "gpt-5.3-codex"}}) +
		event("response.reasoning_summary_text.delta", map[string]any{"delta": "thinking"}) +
		event("response.output_text.delta", map[string]any{"delta": "Hello "}) +
		event("response.output_text.delta", map[string]any{"delta": "world"}) +
		event("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "weather"}}) +
		event("response.function_call_arguments.delta", map[string]any{"item_id": "fc_1", "output_index": 1, "delta": "{\"city\":"}) +
		event("response.function_call_arguments.delta", map[string]any{"item_id": "fc_1", "output_index": 1, "delta": "\"Rome\"}"}) +
		event("response.output_item.done", map[string]any{"output_index": 1, "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "weather", "arguments": "{\"city\":\"Rome\"}"}}) +
		event("response.completed", map[string]any{"response": map[string]any{"id": "resp_1", "model": "gpt-5.3-codex", "status": "completed", "usage": map[string]any{"input_tokens": 10, "output_tokens": 7, "total_tokens": 17, "input_tokens_details": map[string]any{"cached_tokens": 3}, "output_tokens_details": map[string]any{"reasoning_tokens": 2}}}})
}

func TestProviderHeadersBodyAndStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		wantHeaders := map[string]string{
			"Authorization":      "Bearer access-token",
			"chatgpt-account-id": "account-1",
			"Version":            ClientVersion,
			"originator":         Originator,
			"User-Agent":         UserAgent,
			"OpenAI-Beta":        BetaVersion,
			"Content-Type":       "application/json",
			"Accept":             "text/event-stream",
		}
		for key, want := range wantHeaders {
			if got := r.Header.Get(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		if sessionID := r.Header.Get("session-id"); sessionID == "" || r.Header.Get("x-client-request-id") != sessionID {
			t.Errorf("session headers = %q / %q", sessionID, r.Header.Get("x-client-request-id"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["stream"] != true || body["store"] != false {
			t.Fatalf("body = %#v", body)
		}
		if _, ok := body["temperature"]; ok {
			t.Fatal("unsupported field forwarded")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, codexTestStream())
	}))
	defer server.Close()

	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access-token", RefreshToken: "refresh-token", AccountID: "account-1", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL + "/responses", Timeout: time.Second}, store)
	raw := []byte(`{"messages":[{"role":"user","content":"hi"}],"temperature":1}`)
	rr := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gpt-5.3-codex", Stream: true, Raw: raw}, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Header().Get("Content-Type") != "text/event-stream" || !strings.HasSuffix(rr.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("stream response = %s", rr.Body.String())
	}
	for _, part := range []string{`"role":"assistant"`, `"reasoning_content":"thinking"`, `"content":"Hello "`, `"name":"weather"`, `"arguments":"{\"city\":"`, `"arguments":"\"Rome\"}"`, `"finish_reason":"tool_calls"`, `"prompt_tokens":10`} {
		if !strings.Contains(rr.Body.String(), part) {
			t.Errorf("stream missing %s: %s", part, rr.Body.String())
		}
	}
}

func TestProviderNonStreamingAggregatesEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, codexTestStream())
	}))
	defer server.Close()

	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL, Timeout: time.Second}, store)
	rr := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gpt-5.3-codex", Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, rr); err != nil {
		t.Fatal(err)
	}
	var body struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != "resp_1" || body.Object != "chat.completion" || body.Model != "gpt-5.3-codex" {
		t.Fatalf("response = %+v", body)
	}
	choice := body.Choices[0]
	if choice.Message.Role != "assistant" || choice.Message.Content != "Hello world" || choice.Message.ReasoningContent != "thinking" || choice.FinishReason != "tool_calls" {
		t.Fatalf("choice = %+v", choice)
	}
	if len(choice.Message.ToolCalls) != 1 || choice.Message.ToolCalls[0].ID != "call_1" || choice.Message.ToolCalls[0].Function.Arguments != `{"city":"Rome"}` {
		t.Fatalf("tool calls = %+v", choice.Message.ToolCalls)
	}
	if body.Usage.PromptTokens != 10 || body.Usage.CompletionTokens != 7 || body.Usage.TotalTokens != 17 {
		t.Fatalf("usage = %+v", body.Usage)
	}
}

func TestProviderRefreshesOnceOnUnauthorized(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	var inferenceCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "refresh_token": "rotated", "expires_in": 3600})
			return
		}
		call := inferenceCalls.Add(1)
		if call == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"token":"access-old"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fresh" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, event("response.completed", map[string]any{"response": map[string]any{"status": "completed"}}))
	}))
	defer server.Close()
	tokenURL = server.URL + "/token"
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access-old", RefreshToken: "refresh-old", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL + "/responses", Timeout: time.Second}, store)
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gpt", Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, httptest.NewRecorder()); err != nil {
		t.Fatal(err)
	}
	if inferenceCalls.Load() != 2 || store.puts != 1 || store.Get().RefreshToken != "rotated" {
		t.Fatalf("calls = %d, puts = %d, creds = %+v", inferenceCalls.Load(), store.puts, store.Get())
	}
}

func TestProviderConcurrentUnauthorizedUsesOneRefresh(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshCalls.Add(1)
			time.Sleep(20 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "refresh_token": "rotated", "expires_in": 3600})
			return
		}
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, event("response.completed", map[string]any{"response": map[string]any{"status": "completed"}}))
	}))
	defer server.Close()
	tokenURL = server.URL + "/token"
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "old", RefreshToken: "refresh", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL + "/responses", Timeout: time.Second}, store)
	const workers = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gpt", Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, httptest.NewRecorder())
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d", refreshCalls.Load())
	}
}

func TestProviderErrorsAreSanitized(t *testing.T) {
	secret := "secret-upstream-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"`+secret+`"}`)
	}))
	defer server.Close()
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL, Timeout: time.Second}, store)
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gpt", Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, httptest.NewRecorder())
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("error = %v", err)
	}
}

func TestProviderMalformedSSEAndCancellation(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "data: {not-json}\n\n")
		}))
		defer server.Close()
		store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
		p := New(provider.Config{Name: "codex", BaseURL: server.URL, Timeout: time.Second}, store)
		err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gpt", Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, httptest.NewRecorder())
		if err == nil || !strings.Contains(err.Error(), "malformed SSE") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		}))
		store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
		p := New(provider.Config{Name: "codex", BaseURL: server.URL, Timeout: time.Second}, store)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(20*time.Millisecond, cancel)
		err := p.ChatCompletion(ctx, provider.ChatRequest{Model: "gpt", Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, httptest.NewRecorder())
		server.Close()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestProviderReturnsCanceledContextBeforeRequest(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex", BaseURL: "http://127.0.0.1:1"}, store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.ChatCompletion(ctx, provider.ChatRequest{Model: "gpt", Raw: []byte(`{"messages":[]}`)}, httptest.NewRecorder())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestProviderIncompleteResponseUsesLengthFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, event("response.incomplete", map[string]any{"response": map[string]any{"status": "incomplete", "usage": map[string]any{"input_tokens": 2, "output_tokens": 3}}}))
	}))
	defer server.Close()
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL, Timeout: time.Second}, store)
	rr := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gpt", Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, rr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Body.String(), `"finish_reason":"length"`) {
		t.Fatalf("response = %s", rr.Body.String())
	}
}

func TestProviderModelsUsesLiveCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models.json" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
			{"slug": "gpt-live", "display_name": "GPT Live", "visibility": "list", "supported_in_api": true},
			{"slug": "gpt-hidden", "visibility": "hide", "supported_in_api": true},
			{"slug": "gpt-cli-only", "visibility": "list", "supported_in_api": false},
		}})
	}))
	defer server.Close()
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex"}, store).(*Provider)
	p.modelsURL = server.URL + "/models.json"
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "gpt-live" || models[0].Name != "GPT Live" {
		t.Fatalf("models = %+v", models)
	}
}

func TestProviderModelsRetainsFallbackWhenCatalogUnavailableOrMalformed(t *testing.T) {
	for _, response := range []string{"not json", `{"models":[]}`} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, response)
			}))
			defer server.Close()
			store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
			p := New(provider.Config{Name: "codex"}, store).(*Provider)
			p.modelsURL = server.URL
			models, err := p.Models(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != len(DefaultModels) || models[0].ID != DefaultModels[0] {
				t.Fatalf("models = %+v", models)
			}
		})
	}
}

func TestProviderTestRequiresCompleteCredentials(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex"}, store)
	result := p.Test(context.Background())
	if result.OK || result.Error != "codex: account ID is missing" {
		t.Fatalf("result = %+v", result)
	}
}

func TestProviderFailedEventIsSanitized(t *testing.T) {
	secret := "upstream-secret-detail"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, event("response.failed", map[string]any{"response": map[string]any{"error": map[string]any{"message": secret}}}))
	}))
	defer server.Close()
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL, Timeout: time.Second}, store)
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gpt", Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, httptest.NewRecorder())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %v", err)
	}
}

func TestProviderReusesSessionIDAcrossRetry(t *testing.T) {
	oldURL := tokenURL
	t.Cleanup(func() { tokenURL = oldURL })
	var calls atomic.Int32
	var mu sync.Mutex
	sessionIDs := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "refresh_token": "rotated", "expires_in": 3600})
			return
		}
		call := calls.Add(1)
		mu.Lock()
		sessionIDs = append(sessionIDs, r.Header.Get("session-id"))
		mu.Unlock()
		if call == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, event("response.completed", map[string]any{"response": map[string]any{"status": "completed"}}))
	}))
	defer server.Close()
	tokenURL = server.URL + "/token"
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access-old", RefreshToken: "refresh-old", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL + "/responses", Timeout: time.Second}, store)
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "gpt", Raw: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, httptest.NewRecorder()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sessionIDs) != 2 || sessionIDs[0] == "" || sessionIDs[0] != sessionIDs[1] {
		t.Fatalf("session ids = %q", sessionIDs)
	}
}
