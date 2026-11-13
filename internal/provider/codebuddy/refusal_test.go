package codebuddy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type staticStore struct{ creds provider.Credentials }

func (s staticStore) Get() provider.Credentials                     { return s.creds }
func (s staticStore) Put(provider.Credentials) error                { return nil }
func (s staticStore) Accounts() []provider.Credentials              { return []provider.Credentials{s.creds} }
func (s staticStore) PutAccount(string, provider.Credentials) error { return nil }

func newRefusalProvider(t *testing.T, handler http.HandlerFunc) *codebuddyProvider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return New(
		provider.Config{Name: "cb", BaseURL: server.URL, Timeout: 5 * time.Second},
		staticStore{creds: provider.Credentials{APIKey: "k"}},
		false,
	).(*codebuddyProvider)
}

func statusOf(err error) (int, bool) {
	var status interface{ HTTPStatus() int }
	if !errors.As(err, &status) {
		return 0, false
	}
	return status.HTTPStatus(), true
}

// A small in-body refusal on a 200 is still recognized after the read became a
// bounded sniff, so it is not forwarded as a generation.
func TestChatDetectsSmallInBodyRefusal(t *testing.T) {
	p := newRefusalProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":6004,"msg":"frequency limit"}`))
	})
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "glm-5.2", Raw: []byte(`{}`)}, httptest.NewRecorder())
	if code, ok := statusOf(err); !ok || code != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want a typed 429 refusal", err)
	}
}

// A generation longer than the sniff window must be forwarded whole and
// byte for byte, and must not be mistaken for a refusal.
func TestChatForwardsAGenerationPastTheSniffWindow(t *testing.T) {
	var body strings.Builder
	for body.Len() < 2*maxRefusalBytes {
		body.WriteString(`data: {"choices":[{"delta":{"content":"frequency limit"}}]}` + "\n\n")
	}
	payload := body.String()
	p := newRefusalProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(payload))
	})
	rec := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "glm-5.2", Raw: []byte(`{}`)}, rec); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if got := rec.Body.String(); got != payload {
		t.Fatalf("forwarded %d bytes, want %d verbatim", len(got), len(payload))
	}
}

// An error envelope larger than the sniff window cannot be decoded, so it must
// fail the target: forwarding it would hand a refusal to the client as if it
// were the answer.
func TestChatLargeErrorEnvelopeFailsTheTarget(t *testing.T) {
	p := newRefusalProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":6004,"msg":"`))
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxRefusalBytes))
		_, _ = w.Write([]byte(`"}`))
	})
	rec := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{Model: "glm-5.2", Raw: []byte(`{}`)}, rec)
	if err == nil {
		t.Fatalf("forwarded an unreadable error envelope as success: %q", rec.Body.String())
	}
	if code, ok := statusOf(err); ok && code == http.StatusTooManyRequests {
		t.Fatal("a truncated envelope was decoded as a refusal")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("wrote %d bytes before failing", rec.Body.Len())
	}
}
