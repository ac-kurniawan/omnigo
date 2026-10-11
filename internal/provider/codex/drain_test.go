package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// A quota failure on an effort clone (gpt-5.5-high) must cool the base family,
// because availability is looked up under the base model. The key MarkFailed
// writes and the key AvailableForModel reads therefore have to agree.
func TestChatCompletionDrainsTheBaseModelAfterACloneQuotaFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	store := &memoryCredStore{creds: provider.Credentials{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		AccountID:    "account-1",
		ExpiresAt:    time.Now().Add(time.Hour),
	}}
	p := New(provider.Config{Name: "codex", BaseURL: server.URL + "/responses", Timeout: time.Second}, store).(*Provider)

	if healthy, _ := p.pool.AvailableForModel(p.store, "gpt-5.5"); len(healthy) != 1 {
		t.Fatalf("baseline AvailableForModel(gpt-5.5) = %d accounts, want 1", len(healthy))
	}

	rr := httptest.NewRecorder()
	err := p.ChatCompletion(context.Background(), provider.ChatRequest{
		Model:  "gpt-5.5-high",
		Stream: true,
		Raw:    []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}, rr)
	if err == nil {
		t.Fatal("expected the 429 to surface as an error")
	}

	healthy, _ := p.pool.AvailableForModel(p.store, "gpt-5.5")
	if len(healthy) != 0 {
		t.Fatalf("after a 429 on gpt-5.5-high, AvailableForModel(gpt-5.5) = %d accounts, want 0 (base family drained)", len(healthy))
	}
}
