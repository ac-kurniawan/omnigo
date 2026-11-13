package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// firstFrameLatency runs one streaming combo request against a provider that
// emits reasoning for roughly holdDelay before its first answer token, and
// reports how long the client waits for its first byte.
func firstFrameLatency(t *testing.T, holdDelay time.Duration) time.Duration {
	t.Helper()
	provider.Register("openai", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &responseProvider{name: cfg.Name, chat: func(ctx context.Context, _ provider.ChatRequest, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n"))
			flusher.Flush()
			select {
			case <-time.After(holdDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n"))
			flusher.Flush()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return nil
		}}
	})
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "solo", Type: "openai", Models: []string{"m1"}}},
		Combos:    []config.Combo{{Name: "solo-combo", Strategy: "priority", Targets: []config.ComboTarget{{Provider: "solo", Model: "m1"}}}},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}

	// A real server: the client has to observe when bytes actually cross the
	// socket, which a recorder cannot show.
	srv := httptest.NewServer(NewRouter(
		func() *config.Config { return cfg },
		vault.NewMemoryStore(v),
		nil, combo.NewTracker(""), "test-version", nil, nil,
	))
	defer srv.Close()

	body := `{"model":"solo-combo","stream":true,"messages":[]}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	return time.Since(start)
}

// A single-target combo has nothing to fail over to, so holding a stream back
// until a reply frame commits buys nothing and costs the whole thinking phase:
// a client sees no bytes at all while the model is still reasoning.
func TestSingleTargetComboForwardsTheFirstFrame(t *testing.T) {
	const hold = 600 * time.Millisecond
	if got := firstFrameLatency(t, hold); got > hold/2 {
		t.Fatalf("first byte arrived after %s; the reasoning phase (~%s) was buffered", got.Round(10*time.Millisecond), hold)
	}
}
