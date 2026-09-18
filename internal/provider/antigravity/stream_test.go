package antigravity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// The budget is armed per provider, after token refresh, so each provider needs
// its own guard that a trickling upstream cannot outlive it.
func TestChatStreamBudgetEndsTricklingStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	p := New(provider.Config{Name: "agy", BaseURL: srv.URL, Timeout: 5 * time.Second, StreamTimeout: 150 * time.Millisecond},
		staticStore{provider.Credentials{AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})

	done := make(chan error, 1)
	go func() {
		done <- p.ChatCompletion(context.Background(), provider.ChatRequest{
			Model: "gemini-3.7-flash-medium", Stream: true,
			Messages: []provider.Message{{Role: "user", Content: "hi"}},
		}, newProbeRecorder())
	}()

	select {
	case err := <-done:
		if !errors.Is(err, provider.ErrUpstreamStall) {
			t.Fatalf("err = %v, want ErrUpstreamStall from the exhausted stream budget", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("trickling stream was not ended by its budget")
	}
}
