package commandcode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type memoryCredStore struct {
	creds provider.Credentials
}

func (m *memoryCredStore) Get() provider.Credentials { return m.creds }
func (m *memoryCredStore) Put(c provider.Credentials) error {
	m.creds = c
	return nil
}

type poolCredStore struct {
	accounts []provider.Credentials
}

func (s *poolCredStore) Get() provider.Credentials {
	if len(s.accounts) == 0 {
		return provider.Credentials{}
	}
	return s.accounts[0]
}

func (s *poolCredStore) Put(c provider.Credentials) error {
	s.accounts = []provider.Credentials{c}
	return nil
}

func (s *poolCredStore) Accounts() []provider.Credentials {
	return append([]provider.Credentials(nil), s.accounts...)
}

func (s *poolCredStore) PutAccount(identity string, credentials provider.Credentials) error {
	for i := range s.accounts {
		if s.accounts[i].Identity() == identity {
			s.accounts[i] = credentials
			return nil
		}
	}
	s.accounts = append(s.accounts, credentials)
	return nil
}

func statusCode(t *testing.T, err error) int {
	t.Helper()
	var statusErr *provider.HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected *provider.HTTPStatusError, got %T: %v", err, err)
	}
	return statusErr.StatusCode
}

func chatReq(model string) provider.ChatRequest {
	return provider.ChatRequest{
		Model: model,
		Raw:   []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`),
	}
}

func TestCommandCodeMissingKey(t *testing.T) {
	p := New(provider.Config{Name: "cmc"}, &memoryCredStore{})
	rec := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), chatReq("deepseek/deepseek-v4-pro"), rec)
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
	if got := statusCode(t, err); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", got)
	}
}

func TestCommandCodeAuthHeadersAndForcedStream(t *testing.T) {
	var capturedHeader http.Header
	var capturedBody map[string]any

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"hello\"}\n"))
		_, _ = w.Write([]byte("{\"type\":\"finish\",\"finishReason\":\"stop\"}\n"))
	}))
	defer ts.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "user_test123"}}
	p := New(provider.Config{Name: "cmc", BaseURL: ts.URL, Timeout: 5 * time.Second}, store)

	rec := httptest.NewRecorder()
	req := chatReq("deepseek/deepseek-v4-pro")
	req.Stream = true
	if err := p.ChatCompletion(context.Background(), req, rec); err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	if got := capturedHeader.Get("Authorization"); got != "Bearer user_test123" {
		t.Errorf("Authorization = %q, want Bearer user_test123", got)
	}
	for _, h := range []string{"User-Agent", "X-Command-Code-Version", "X-Cli-Environment", "X-Cli-Surface", "X-Project-Slug", "X-Session-Id"} {
		if capturedHeader.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	params, ok := capturedBody["params"].(map[string]any)
	if !ok {
		t.Fatalf("missing params envelope, got %v", capturedBody)
	}
	if params["stream"] != true {
		t.Errorf("params.stream = %v, want true (forced)", params["stream"])
	}
	if capturedBody["threadId"] == "" {
		t.Error("expected threadId in envelope")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: ") || !strings.Contains(body, "[DONE]") {
		t.Errorf("expected OpenAI SSE with [DONE], got %q", body)
	}
	if !strings.Contains(body, "hello") {
		t.Errorf("expected translated text in SSE, got %q", body)
	}
}

func TestCommandCodeErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized},
		{"rate limit", http.StatusTooManyRequests, `{"error":{"message":"rate limit exceeded"}}`, http.StatusTooManyRequests},
		{"server error", http.StatusBadGateway, `{"error":{"message":"overloaded"}}`, http.StatusBadGateway},
		{"ndjson error event", http.StatusOK, "{\"type\":\"error\",\"error\":{\"message\":\"quota exhausted\"}}\n", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			store := &memoryCredStore{creds: provider.Credentials{APIKey: "user_test123"}}
			p := New(provider.Config{Name: "cmc", BaseURL: ts.URL, Timeout: 5 * time.Second}, store)
			rec := httptest.NewRecorder()
			err := p.ChatCompletion(context.Background(), chatReq("x/y"), rec)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if got := statusCode(t, err); got != tc.wantStatus {
				t.Fatalf("status = %d, want %d", got, tc.wantStatus)
			}
		})
	}
}

func TestCommandCodeModelsFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "user_test123"}}
	p := New(provider.Config{Name: "cmc", BaseURL: ts.URL}, store)
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models should fall back to static catalog, got error: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("expected non-empty static model catalog")
	}
}

func TestCommandCodeMultiAccountFailover(t *testing.T) {
	var usedTokens []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		usedTokens = append(usedTokens, token)
		if token == "user-bad" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"ok\"}\n"))
		_, _ = w.Write([]byte("{\"type\":\"finish\",\"finishReason\":\"stop\"}\n"))
	}))
	defer ts.Close()

	store := &poolCredStore{accounts: []provider.Credentials{
		{AccountID: "a1", APIKey: "user-bad"},
		{AccountID: "a2", APIKey: "user-good"},
	}}
	p := New(provider.Config{Name: "cmc", BaseURL: ts.URL}, store)
	rec := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), chatReq("x/y"), rec); err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}
	if len(usedTokens) != 2 || usedTokens[0] != "user-bad" || usedTokens[1] != "user-good" {
		t.Errorf("expected failover user-bad -> user-good, got %v", usedTokens)
	}
}

func TestCommandCodeTestRequiresKey(t *testing.T) {
	p := New(provider.Config{Name: "cmc"}, &memoryCredStore{})
	res := p.Test(context.Background())
	if res.OK {
		t.Fatal("Test must fail without a key")
	}
}

func TestCommandCodeTestRejectsBadKey(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer ts.Close()

	p := New(provider.Config{Name: "cmc", BaseURL: ts.URL, Timeout: 5 * time.Second},
		&memoryCredStore{creds: provider.Credentials{APIKey: "user_bad"}})
	if res := p.Test(context.Background()); res.OK {
		t.Fatal("Test must fail for a rejected key")
	}
}

func TestCommandCodeTestAcceptsGoodKey(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"type\":\"finish\",\"finishReason\":\"stop\"}\n"))
	}))
	defer ts.Close()

	p := New(provider.Config{Name: "cmc", BaseURL: ts.URL, Timeout: 5 * time.Second},
		&memoryCredStore{creds: provider.Credentials{APIKey: "user_ok"}})
	res := p.Test(context.Background())
	if !res.OK {
		t.Fatalf("Test must pass for an accepted key, got error %q", res.Error)
	}
	if res.Error != "" {
		t.Fatalf("Error = %q, want empty", res.Error)
	}
}

func TestCommandCodeTestProbeUsesConfiguredModel(t *testing.T) {
	var capturedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"type\":\"finish\",\"finishReason\":\"stop\"}\n"))
	}))
	defer ts.Close()

	p := New(provider.Config{Name: "cmc", BaseURL: ts.URL, Timeout: 5 * time.Second, Models: []string{"probe-model-id"}},
		&memoryCredStore{creds: provider.Credentials{APIKey: "user_ok"}})
	if res := p.Test(context.Background()); !res.OK {
		t.Fatalf("Test must pass, got error %q", res.Error)
	}

	params, ok := capturedBody["params"].(map[string]any)
	if !ok {
		t.Fatalf("missing params envelope, got %v", capturedBody)
	}
	if got := params["model"]; got != "probe-model-id" {
		t.Fatalf("probe model = %v, want probe-model-id", got)
	}
}

func TestCommandCodeUsageNeverReportsAPIKeyAsAccount(t *testing.T) {
	const key = "user_secret_key_value_1234"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"hi\"}\n"))
		_, _ = w.Write([]byte("{\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":3,\"outputTokens\":2}}\n"))
	}))
	defer ts.Close()

	p := New(provider.Config{Name: "cmc", BaseURL: ts.URL, Timeout: 5 * time.Second},
		&memoryCredStore{creds: provider.Credentials{APIKey: key}})

	var got []provider.TokenUsage
	ctx := provider.WithUsageSink(context.Background(), func(u provider.TokenUsage) {
		got = append(got, u)
	})
	rec := httptest.NewRecorder()
	req := chatReq("deepseek/deepseek-v4-pro")
	req.Stream = true
	if err := p.ChatCompletion(ctx, req, rec); err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("usage reports = %d, want 1", len(got))
	}
	if got[0].Account == key {
		t.Fatalf("usage Account leaked the API key %q", key)
	}
	if strings.Contains(got[0].Account, key) {
		t.Fatalf("usage Account %q contains the API key", got[0].Account)
	}
}
