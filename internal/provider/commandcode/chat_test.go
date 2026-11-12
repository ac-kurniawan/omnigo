package commandcode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type memStore struct {
	accounts []provider.Credentials
}

func (m *memStore) Get() provider.Credentials {
	if len(m.accounts) == 0 {
		return provider.Credentials{}
	}
	return m.accounts[0]
}

func (m *memStore) Put(c provider.Credentials) error {
	if len(m.accounts) == 0 {
		m.accounts = append(m.accounts, c)
	} else {
		m.accounts[0] = c
	}
	return nil
}

func (m *memStore) Accounts() []provider.Credentials { return m.accounts }

func (m *memStore) PutAccount(identity string, c provider.Credentials) error {
	for i, a := range m.accounts {
		if a.Identity() == identity {
			m.accounts[i] = c
			return nil
		}
	}
	m.accounts = append(m.accounts, c)
	return nil
}

func chatReq(t *testing.T, model string, stream bool) provider.ChatRequest {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   stream,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	parsed := provider.ParseBody(raw)
	return provider.ChatRequest{Model: model, Stream: stream, Parsed: parsed, Raw: raw}
}

func TestChatNotAuthenticated(t *testing.T) {
	p := New(provider.Config{Name: "cc"}, &memStore{}).(*Provider)
	w := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), chatReq(t, "m", true), w)
	if err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("err = %v", err)
	}
}

func TestChatStreamAndHeaders(t *testing.T) {
	var gotAuth, gotSession, gotSlug, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("x-session-id")
		gotSlug = r.Header.Get("x-project-slug")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"hello\"}\n{\"type\":\"finish\"}\n"))
	}))
	defer srv.Close()
	store := &memStore{accounts: []provider.Credentials{{APIKey: "k", AccountID: "u1"}}}
	p := New(provider.Config{Name: "cc", BaseURL: srv.URL}, store).(*Provider)
	w := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), chatReq(t, "m", true), w); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if gotAuth != "Bearer k" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotSession == "" || gotSlug == "" {
		t.Fatalf("session=%q slug=%q", gotSession, gotSlug)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if body["threadId"] != gotSession {
		t.Fatalf("threadId != x-session-id: %v vs %v", body["threadId"], gotSession)
	}
	out := w.Body.String()
	if !strings.Contains(out, `"content":"hello"`) || !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("SSE output = %q", out)
	}
}

func TestChatNonStreamAggregates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"hi\"}\n{\"type\":\"finish\",\"item\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}}\n"))
	}))
	defer srv.Close()
	store := &memStore{accounts: []provider.Credentials{{APIKey: "k", AccountID: "u1"}}}
	p := New(provider.Config{Name: "cc", BaseURL: srv.URL}, store).(*Provider)
	w := httptest.NewRecorder()
	var usage provider.TokenUsage
	ctx := provider.WithUsageSink(context.Background(), func(u provider.TokenUsage) { usage = u })
	if err := p.ChatCompletion(ctx, chatReq(t, "m", false), w); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v body=%q", err, w.Body.String())
	}
	if resp["object"] != "chat.completion" {
		t.Fatalf("object = %v", resp["object"])
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "hi" {
		t.Fatalf("content = %v", msg)
	}
	u := resp["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(3) || u["completion_tokens"] != float64(4) || u["total_tokens"] != float64(7) {
		t.Fatalf("usage = %v", u)
	}
	if !usage.Present || usage.InputTokens != 3 || usage.Account != "u1" {
		t.Fatalf("sink usage = %#v", usage)
	}
}

func TestChatRetryBeforeCommit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"ok\"}\n{\"type\":\"finish\"}\n"))
	}))
	defer srv.Close()
	store := &memStore{accounts: []provider.Credentials{{APIKey: "k"}}}
	p := New(provider.Config{Name: "cc", BaseURL: srv.URL}, store).(*Provider)
	w := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), chatReq(t, "m", true), w); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("calls = %d, want 2 (retry before commit)", calls)
	}
}

func TestChatAuthAndRateLimitMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   int
	}{
		{http.StatusUnauthorized, http.StatusUnauthorized},
		{http.StatusTooManyRequests, http.StatusTooManyRequests},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		store := &memStore{accounts: []provider.Credentials{{APIKey: "k"}}}
		p := New(provider.Config{Name: "cc", BaseURL: srv.URL}, store).(*Provider)
		err := p.ChatCompletion(context.Background(), chatReq(t, "m", true), httptest.NewRecorder())
		he, ok := err.(*provider.HTTPStatusError)
		if !ok {
			t.Fatalf("status %d: err type %T (%v)", tc.status, err, err)
		}
		if he.StatusCode != tc.want {
			t.Fatalf("status %d: got %d", tc.status, he.StatusCode)
		}
		srv.Close()
	}
}

func TestChatFailoverToSecondAccount(t *testing.T) {
	var first bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !first {
			first = true
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"second\"}\n{\"type\":\"finish\"}\n"))
	}))
	defer srv.Close()
	store := &memStore{accounts: []provider.Credentials{{APIKey: "bad", AccountID: "a1"}, {APIKey: "good", AccountID: "a2"}}}
	p := New(provider.Config{Name: "cc", BaseURL: srv.URL}, store).(*Provider)
	w := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), chatReq(t, "m", true), w); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if !strings.Contains(w.Body.String(), "second") {
		t.Fatalf("expected failover output, got %q", w.Body.String())
	}
}
