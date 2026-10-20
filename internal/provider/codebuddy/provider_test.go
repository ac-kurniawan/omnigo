package codebuddy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

type memoryCredStore struct {
	creds provider.Credentials
}

func (m *memoryCredStore) Get() provider.Credentials {
	return m.creds
}

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

func TestCodebuddyProviderMultiAccountFailover(t *testing.T) {
	callCount := 0
	var usedTokens []string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		usedTokens = append(usedTokens, token)

		if token == "key-bad" {
			// First key returns 429 rate limit (6004)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"code":6004,"msg":"超出频率限制"}`))
			return
		}

		// Second key succeeds
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok from good key\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	store := &poolCredStore{
		accounts: []provider.Credentials{
			{AccountID: "acc-1", APIKey: "key-bad"},
			{AccountID: "acc-2", APIKey: "key-good"},
		},
	}

	p := New(provider.Config{
		Name:    "codebuddy-intl",
		BaseURL: ts.URL,
	}, store, true)

	rec := httptest.NewRecorder()
	req := provider.ChatRequest{
		Model: "glm-5.2",
		Raw:   []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
	}

	err := p.ChatCompletion(context.Background(), req, rec)
	if err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	if callCount != 2 {
		t.Fatalf("expected 2 calls (failover), got %d", callCount)
	}
	if len(usedTokens) != 2 || usedTokens[0] != "key-bad" || usedTokens[1] != "key-good" {
		t.Errorf("expected failover from key-bad to key-good, got tokens: %v", usedTokens)
	}
}

func TestCodebuddyProviderChatCompletionHeadersAndStream(t *testing.T) {
	var capturedHeader http.Header
	var capturedBody map[string]any

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Clone()
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &capturedBody)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	store := &memoryCredStore{
		creds: provider.Credentials{
			APIKey: "cb-test-token",
		},
	}

	p := New(provider.Config{
		Name:    "cb-test",
		BaseURL: ts.URL,
		Timeout: 5 * time.Second,
	}, store, false)

	rec := httptest.NewRecorder()
	req := provider.ChatRequest{
		Model:  "glm-5.2",
		Stream: false, // client asked for non-stream, but CodeBuddy upstream must force stream = true
		Raw:    []byte(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`),
	}

	err := p.ChatCompletion(context.Background(), req, rec)
	if err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	// Verify headers
	if capturedHeader.Get("Authorization") != "Bearer cb-test-token" {
		t.Errorf("expected Authorization Bearer cb-test-token, got %s", capturedHeader.Get("Authorization"))
	}
	if capturedHeader.Get("x-codebuddy-request") != "1" {
		t.Errorf("expected x-codebuddy-request: 1")
	}
	if capturedHeader.Get("X-Product") != "SaaS" {
		t.Errorf("expected X-Product: SaaS")
	}
	if capturedHeader.Get("X-Requested-With") != "XMLHttpRequest" {
		t.Errorf("expected X-Requested-With: XMLHttpRequest")
	}
	if !strings.Contains(capturedHeader.Get("User-Agent"), "CodeBuddy") {
		t.Errorf("expected User-Agent containing CodeBuddy, got %s", capturedHeader.Get("User-Agent"))
	}

	// Verify upstream forced stream: true
	if streamVal, ok := capturedBody["stream"].(bool); !ok || !streamVal {
		t.Errorf("expected upstream body stream=true, got %v", capturedBody["stream"])
	}
}

func TestCodebuddyIntlTransformsPayloadAndHeaders(t *testing.T) {
	var capturedHeader http.Header
	var capturedBody map[string]any

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Clone()
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &capturedBody)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-intl-token"}}
	// BaseURL containing codebuddy.ai should trigger intl mode
	p := New(provider.Config{
		Name:    "codebuddy-intl",
		BaseURL: ts.URL,
	}, store, true)

	req := provider.ChatRequest{
		Model: "glm-5.2",
		Raw:   []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello codebuddy"}]}`),
	}

	rec := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), req, rec)
	if err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	// Intl headers check
	if capturedHeader.Get("X-Domain") != "www.codebuddy.ai" {
		t.Errorf("expected X-Domain www.codebuddy.ai, got %s", capturedHeader.Get("X-Domain"))
	}
	if !strings.HasPrefix(capturedHeader.Get("User-Agent"), "IDE/") {
		t.Errorf("expected User-Agent starting with IDE/, got %s", capturedHeader.Get("User-Agent"))
	}
	if capturedHeader.Get("X-IDE-Type") != "IDE" {
		t.Errorf("expected X-IDE-Type: IDE, got %s", capturedHeader.Get("X-IDE-Type"))
	}

	// Messages format check: leading system prompt + typed block content
	msgs, ok := capturedBody["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %v", msgs)
	}
	sysMsg := msgs[0].(map[string]any)
	if sysMsg["role"] != "system" || sysMsg["content"] != "You are CodeBuddy Code." {
		t.Errorf("expected system prompt 'You are CodeBuddy Code.', got %v", sysMsg)
	}
	userMsg := msgs[1].(map[string]any)
	if userMsg["role"] != "user" {
		t.Errorf("expected user role")
	}
	contentBlocks, ok := userMsg["content"].([]any)
	if !ok || len(contentBlocks) != 1 {
		t.Fatalf("expected content as typed blocks array, got %v", userMsg["content"])
	}
	block := contentBlocks[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hello codebuddy" {
		t.Errorf("expected typed text block, got %v", block)
	}
}

func TestCodebuddyProviderNeutralizesAgentSystemPrompt(t *testing.T) {
	var capturedBody map[string]any

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &capturedBody)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	store := &memoryCredStore{
		creds: provider.Credentials{APIKey: "cb-token"},
	}

	p := New(provider.Config{
		Name:    "cb-test",
		BaseURL: ts.URL,
	}, store, false)

	agentPrompt := "You are Claude Code, Anthropic's official CLI for software development..."
	req := provider.ChatRequest{
		Model:  "deepseek-v4.1-flash",
		Stream: true,
		Raw:    []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"` + agentPrompt + `"},{"role":"user","content":"help me fix bug"}]}`),
	}

	rec := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), req, rec)
	if err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	msgs, ok := capturedBody["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("expected messages array in captured body")
	}
	firstMsg := msgs[0].(map[string]any)
	if firstMsg["role"] != "system" {
		t.Errorf("expected system role")
	}
	contentStr, ok := firstMsg["content"].(string)
	if !ok {
		t.Fatalf("expected content string")
	}
	if strings.Contains(contentStr, "Claude Code") {
		t.Errorf("system prompt was not neutralized: %s", contentStr)
	}
	if contentStr != NeutralSystemPrompt {
		t.Errorf("expected neutral prompt %q, got %q", NeutralSystemPrompt, contentStr)
	}
}

func TestCodebuddyProviderReasoningSummaryHandling(t *testing.T) {
	var capturedBody map[string]any

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &capturedBody)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	store := &memoryCredStore{
		creds: provider.Credentials{APIKey: "cb-token"},
	}
	p := New(provider.Config{Name: "cb-test", BaseURL: ts.URL}, store, false)

	// Case 1: client specifies reasoning_effort -> should set reasoning_summary = "auto"
	reqWithReasoning := provider.ChatRequest{
		Model: "deepseek-v4-pro",
		Raw:   []byte(`{"model":"deepseek-v4-pro","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`),
	}
	_ = p.ChatCompletion(context.Background(), reqWithReasoning, httptest.NewRecorder())
	if capturedBody["reasoning_summary"] != "auto" {
		t.Errorf("expected reasoning_summary: 'auto', got %v", capturedBody["reasoning_summary"])
	}

	// Case 2: client specifies reasoning_effort: "none" -> should delete reasoning_effort and not set reasoning_summary
	capturedBody = nil
	reqNone := provider.ChatRequest{
		Model: "deepseek-v4-pro",
		Raw:   []byte(`{"model":"deepseek-v4-pro","reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`),
	}
	_ = p.ChatCompletion(context.Background(), reqNone, httptest.NewRecorder())
	if _, ok := capturedBody["reasoning_effort"]; ok {
		t.Errorf("expected reasoning_effort to be removed when 'none'")
	}
	if _, ok := capturedBody["reasoning_summary"]; ok {
		t.Errorf("expected reasoning_summary to be omitted when reasoning is none")
	}
}

func TestCodebuddyProviderRateLimitErrorMapping(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // CodeBuddy sometimes returns code 6004 inside body with 200 or 400/429
		w.Write([]byte(`{"code":6004,"msg":"超出频率限制 2026-10-10 15:30:00 UTC+8"}`))
	}))
	defer ts.Close()

	store := &memoryCredStore{
		creds: provider.Credentials{APIKey: "cb-token"},
	}
	p := New(provider.Config{Name: "cb-test", BaseURL: ts.URL}, store, false)

	req := provider.ChatRequest{
		Model: "glm-5.2",
		Raw:   []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
	}

	rec := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), req, rec)
	if err == nil {
		t.Fatalf("expected error on rate limit 6004 response")
	}

	statusErr, ok := err.(*provider.HTTPStatusError)
	if !ok {
		t.Fatalf("expected *provider.HTTPStatusError, got %T: %v", err, err)
	}
	if statusErr.HTTPStatus() != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", statusErr.HTTPStatus())
	}
}

func TestCodebuddyIntlDNSFallbackDialer(t *testing.T) {
	dialedAddr := ""
	customDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialedAddr = addr
		return &mockConn{}, nil
	}

	dialer := newCodebuddyFallbackDialer(customDial)

	// Case 1: host is not www.codebuddy.ai -> pass through untouched
	dialedAddr = ""
	conn, err := dialer(context.Background(), "tcp", "api.openai.com:443")
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	conn.Close()
	if dialedAddr != "api.openai.com:443" {
		t.Errorf("expected dialedAddr api.openai.com:443, got %s", dialedAddr)
	}

	// Case 2: host is www.codebuddy.ai:443, DNS resolves to 0.0.0.1 or fails -> should dial 43.170.214.92:443
	dialedAddr = ""
	conn, err = dialer(context.Background(), "tcp", "www.codebuddy.ai:443")
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	conn.Close()
	if dialedAddr != "43.170.214.92:443" {
		t.Errorf("expected dialedAddr 43.170.214.92:443, got %s", dialedAddr)
	}
}

type mockConn struct {
	net.Conn
}

func (m *mockConn) Close() error { return nil }

func TestCodebuddyProviderModelsFallbackOn404(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":"not_found","message":"Not Found"}}`))
	}))
	defer ts.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-token"}}
	p := New(provider.Config{Name: "codebuddy-intl", BaseURL: ts.URL}, store, true)

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("expected Models() fallback on 404 without error, got %v", err)
	}
	if len(models) != len(DefaultIntlModels) {
		t.Fatalf("expected %d fallback models, got %d", len(DefaultIntlModels), len(models))
	}
	if models[0].ID != "glm-5.2" || models[1].ID != "glm-5.1" || models[2].ID != "deepseek-v3" {
		t.Errorf("expected glm-5.2, glm-5.1, deepseek-v3, got %+v", models)
	}

	testRes := p.Test(context.Background())
	if !testRes.OK {
		t.Errorf("expected Test() to pass with fallback models, got error: %s", testRes.Error)
	}
}

func TestCodebuddyProviderTestResilienceWithoutAPIKey(t *testing.T) {
	p := New(provider.Config{Name: "codebuddy-intl"}, nil, true)
	res := p.Test(context.Background())
	if res.OK {
		t.Errorf("expected Test() to fail when no API key configured")
	}
	if res.Error == "" {
		t.Errorf("expected non-empty error message")
	}
}

func TestCodebuddyProviderModelsDiscovery(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cb-token" {
			t.Errorf("missing Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "glm-5.2"},
				{"id": "minimax-m3"},
				{"id": "deepseek-v4.1-flash"},
			},
		})
	}))
	defer ts.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-token"}}
	p := New(provider.Config{Name: "cb-test", BaseURL: ts.URL}, store, false)

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models failed: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}

	testRes := p.Test(context.Background())
	if !testRes.OK {
		t.Errorf("Test failed: %s", testRes.Error)
	}
}

func TestCodebuddyIntlSendsConversationHeaders(t *testing.T) {
	var capturedHeader http.Header

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-intl-token"}}
	p := New(provider.Config{Name: "codebuddy-intl", BaseURL: ts.URL}, store, true)

	req := provider.ChatRequest{
		Model: "glm-5.2",
		Raw:   []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
	}
	if err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder()); err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	// The intl gateway rejects requests without the X-Conversation-* family
	// with HTTP 400 code 11128 ("channel verification").
	for _, name := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"} {
		if capturedHeader.Get(name) == "" {
			t.Errorf("expected %s to be set for codebuddy-intl", name)
		}
	}
}

func TestCodebuddyCNSendsNoConversationHeaders(t *testing.T) {
	var capturedHeader http.Header

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-cn-token"}}
	p := New(provider.Config{Name: "codebuddy-cn", BaseURL: ts.URL}, store, false)

	req := provider.ChatRequest{
		Model: "glm-5.2",
		Raw:   []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
	}
	if err := p.ChatCompletion(context.Background(), req, httptest.NewRecorder()); err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}

	// The CN endpoint is not gated upstream and must not carry the intl headers.
	for _, name := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"} {
		if capturedHeader.Get(name) != "" {
			t.Errorf("expected %s to be absent for codebuddy-cn, got %q", name, capturedHeader.Get(name))
		}
	}
}

func TestCodebuddyIntlConversationIDIsStablePerSession(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-intl-token"}}
	p := New(provider.Config{Name: "codebuddy-intl", BaseURL: "https://example.invalid"}, store, true).(*codebuddyProvider)

	raw := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"same opening message"}]}`)
	req := provider.ChatRequest{Model: "glm-5.2", Raw: raw, Parsed: provider.ParseBody(raw)}

	first, err := p.conversationIDs(context.Background(), req)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	second, err := p.conversationIDs(context.Background(), req)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	if first.conversationID != second.conversationID {
		t.Errorf("conversation id changed between identical requests: %q vs %q", first.conversationID, second.conversationID)
	}
	if first.requestID == second.requestID {
		t.Errorf("request id must be fresh per request, got %q twice", first.requestID)
	}
	if first.messageID == second.messageID {
		t.Errorf("message id must be fresh per request, got %q twice", first.messageID)
	}
	if first.conversationID == first.requestID || first.conversationID == first.messageID {
		t.Errorf("conversation id must differ from the per-request ids")
	}

	uuidRE := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	for name, value := range map[string]string{
		"conversation": first.conversationID,
		"request":      first.requestID,
		"message":      first.messageID,
	} {
		if !uuidRE.MatchString(value) {
			t.Errorf("expected %s id to be a uuid, got %q", name, value)
		}
	}
}

func TestCodebuddyIntlConversationIDPrefersExplicitSessionKey(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-intl-token"}}
	p := New(provider.Config{Name: "codebuddy-intl", BaseURL: "https://example.invalid"}, store, true).(*codebuddyProvider)

	raw := []byte(`{"model":"glm-5.2","session_id":"sess-123","messages":[{"role":"user","content":"hi"}]}`)
	req := provider.ChatRequest{Model: "glm-5.2", Raw: raw, Parsed: provider.ParseBody(raw)}
	withSession, err := p.conversationIDs(context.Background(), req)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}

	otherRaw := []byte(`{"model":"glm-5.2","session_id":"sess-456","messages":[{"role":"user","content":"hi"}]}`)
	otherReq := provider.ChatRequest{Model: "glm-5.2", Raw: otherRaw, Parsed: provider.ParseBody(otherRaw)}
	other, err := p.conversationIDs(context.Background(), otherReq)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	if withSession.conversationID == other.conversationID {
		t.Errorf("distinct session ids must yield distinct conversation ids")
	}

	// The session id must win over the opening message text: two requests
	// that differ only in text but share a session id keep one conversation.
	sameSessionOtherText := []byte(`{"model":"glm-5.2","session_id":"sess-123","messages":[{"role":"user","content":"a different first message"}]}`)
	sameSessionReq := provider.ChatRequest{Model: "glm-5.2", Raw: sameSessionOtherText, Parsed: provider.ParseBody(sameSessionOtherText)}
	sameSession, err := p.conversationIDs(context.Background(), sameSessionReq)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	if sameSession.conversationID != withSession.conversationID {
		t.Errorf("session id must take precedence over the opening message: %q vs %q", sameSession.conversationID, withSession.conversationID)
	}
}

func TestCodebuddyIntlConversationIDIsStableAcrossGrowingConversation(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-intl-token"}}
	p := New(provider.Config{Name: "codebuddy-intl", BaseURL: "https://example.invalid"}, store, true).(*codebuddyProvider)

	firstRaw := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"opening message"}]}`)
	firstReq := provider.ChatRequest{Model: "glm-5.2", Raw: firstRaw, Parsed: provider.ParseBody(firstRaw)}
	first, err := p.conversationIDs(context.Background(), firstReq)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}

	// The same conversation with more turns must keep the same id, otherwise
	// upstream cannot reuse its prompt cache across the turns.
	grownRaw := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"opening message"},{"role":"assistant","content":"hi"},{"role":"user","content":"second turn"}]}`)
	grownReq := provider.ChatRequest{Model: "glm-5.2", Raw: grownRaw, Parsed: provider.ParseBody(grownRaw)}
	grown, err := p.conversationIDs(context.Background(), grownReq)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	if grown.conversationID != first.conversationID {
		t.Errorf("conversation id changed when the conversation grew: %q vs %q", grown.conversationID, first.conversationID)
	}
}

func TestCodebuddyIntlConversationIDReadsTypedBlockContent(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-intl-token"}}
	p := New(provider.Config{Name: "codebuddy-intl", BaseURL: "https://example.invalid"}, store, true).(*codebuddyProvider)

	raw := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"typed opening message"}]}]}`)
	req := provider.ChatRequest{Model: "glm-5.2", Raw: raw, Parsed: provider.ParseBody(raw)}
	ids, err := p.conversationIDs(context.Background(), req)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	if ids.conversationID == "" {
		t.Fatal("expected a conversation id derived from typed block content")
	}

	otherRaw := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"a different typed message"}]}]}`)
	otherReq := provider.ChatRequest{Model: "glm-5.2", Raw: otherRaw, Parsed: provider.ParseBody(otherRaw)}
	other, err := p.conversationIDs(context.Background(), otherReq)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	if other.conversationID == ids.conversationID {
		t.Error("different typed block messages must yield different conversation ids")
	}
}

func TestCodebuddyIntlConversationIDFallbackIsFresh(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-intl-token"}}
	p := New(provider.Config{Name: "codebuddy-intl", BaseURL: "https://example.invalid"}, store, true).(*codebuddyProvider)

	raw := []byte(`{"model":"glm-5.2","messages":[]}`)
	req := provider.ChatRequest{Model: "glm-5.2", Raw: raw, Parsed: provider.ParseBody(raw)}
	first, err := p.conversationIDs(context.Background(), req)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	second, err := p.conversationIDs(context.Background(), req)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	if first.conversationID == second.conversationID {
		t.Errorf("fallback conversation id must be fresh per request, got %q twice", first.conversationID)
	}
}

func TestCodebuddyIntlConversationIDDoesNotMutateParsedBody(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-intl-token"}}
	p := New(provider.Config{Name: "codebuddy-intl", BaseURL: "https://example.invalid"}, store, true).(*codebuddyProvider)

	raw := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	parsed := provider.ParseBody(raw)
	req := provider.ChatRequest{Model: "glm-5.2", Raw: raw, Parsed: parsed}
	if _, err := p.conversationIDs(context.Background(), req); err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	if _, ok := parsed[provider.OpenAIBodyCacheKey()]; !ok {
		t.Error("the internal body cache slot must survive")
	}
	for key := range parsed {
		if key == provider.OpenAIBodyCacheKey() || key == "model" || key == "messages" {
			continue
		}
		t.Errorf("parsed body gained an unexpected field %q", key)
	}
}

func TestCodebuddyIntlConversationIDFallsBackOnUndecodableRawBody(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-intl-token"}}
	p := New(provider.Config{Name: "codebuddy-intl", BaseURL: "https://example.invalid"}, store, true).(*codebuddyProvider)

	req := provider.ChatRequest{Model: "glm-5.2", Raw: []byte(`not json`)}
	ids, err := p.conversationIDs(context.Background(), req)
	if err != nil {
		t.Fatalf("conversationIDs must not fail on an undecodable body: %v", err)
	}
	if ids.conversationID == "" {
		t.Error("expected a conversation id even when the body cannot be decoded")
	}
}

func TestCodebuddyIntlConversationIDFallsBackWhenNothingDerivable(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-intl-token"}}
	p := New(provider.Config{Name: "codebuddy-intl", BaseURL: "https://example.invalid"}, store, true).(*codebuddyProvider)

	raw := []byte(`{"model":"glm-5.2","messages":[]}`)
	req := provider.ChatRequest{Model: "glm-5.2", Raw: raw, Parsed: provider.ParseBody(raw)}
	ids, err := p.conversationIDs(context.Background(), req)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	for name, value := range map[string]string{
		"conversation": ids.conversationID,
		"request":      ids.requestID,
		"message":      ids.messageID,
	} {
		if value == "" {
			t.Errorf("expected a %s id even with nothing derivable", name)
		}
	}
}

func TestCodebuddyIntlConversationIDIsScopedPerTenant(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-intl-token"}}
	p := New(provider.Config{Name: "codebuddy-intl", BaseURL: "https://example.invalid"}, store, true).(*codebuddyProvider)

	raw := []byte(`{"model":"glm-5.2","session_id":"shared-session","messages":[{"role":"user","content":"hi"}]}`)
	req := provider.ChatRequest{Model: "glm-5.2", Raw: raw, Parsed: provider.ParseBody(raw)}

	tenantA := callerContext(t, "tenant-a")
	tenantB := callerContext(t, "tenant-b")

	a, err := p.conversationIDs(tenantA, req)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	b, err := p.conversationIDs(tenantB, req)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	if a.conversationID == b.conversationID {
		t.Errorf("tenants must not share a conversation id: %q", a.conversationID)
	}

	// The same tenant keeps a stable id across turns.
	aAgain, err := p.conversationIDs(tenantA, req)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	if aAgain.conversationID != a.conversationID {
		t.Errorf("conversation id changed for the same tenant: %q vs %q", aAgain.conversationID, a.conversationID)
	}
}

func TestCodebuddyIntlConversationIDIsSaltedPerProcess(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{APIKey: "cb-intl-token"}}
	raw := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	req := provider.ChatRequest{Model: "glm-5.2", Raw: raw, Parsed: provider.ParseBody(raw)}

	first := New(provider.Config{Name: "codebuddy-intl", BaseURL: "https://example.invalid"}, store, true).(*codebuddyProvider)
	second := New(provider.Config{Name: "codebuddy-intl", BaseURL: "https://example.invalid"}, store, true).(*codebuddyProvider)

	a, err := first.conversationIDs(context.Background(), req)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	b, err := second.conversationIDs(context.Background(), req)
	if err != nil {
		t.Fatalf("conversationIDs: %v", err)
	}
	if a.conversationID == b.conversationID {
		t.Errorf("conversation id must not be reversible to the opening message: %q", a.conversationID)
	}
}

func TestCodebuddyIntlRequestPayloadHandlesNullBody(t *testing.T) {

	payload, err := requestPayload(provider.ChatRequest{Model: "glm-5.2", Raw: []byte(`null`)})
	if err != nil {
		t.Fatalf("requestPayload: %v", err)
	}
	if payload == nil {
		t.Fatal("requestPayload must not return a nil map for a null body")
	}
	payload["model"] = "glm-5.2"
}

// callerContext builds a context carrying the given gateway client key id.
func callerContext(t *testing.T, id string) context.Context {
	t.Helper()
	caller := &auth.Caller{}
	caller.Set(vault.ClientKey{ID: id})
	return auth.WithCaller(context.Background(), caller)
}
