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
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// The gateway must deliver the first SSE chunk to the client while the upstream
// stream is still open. If the combo's buffered writer stops flushing mid-stream
// (or the handler buffers the whole upstream response), the client waits for
// upstream EOF and this test fails by timeout.
func TestComboStreamsFirstChunkBeforeUpstreamEOF(t *testing.T) {
	// Register the real provider: sibling tests replace the global "openai"
	// factory with fakes, so this test must not depend on registration order.
	provider.Register("openai", provider.NewOpenAI)
	firstTokenSent := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n"))
		flusher.Flush()
		close(firstTokenSent)
		<-release // hold the stream open: no further bytes yet
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()
	defer close(release)

	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: upstream.URL, Models: []string{"gpt-4o"}}},
		Combos: []config.Combo{{
			Name: "safe", Strategy: "reliable",
			Targets: []config.ComboTarget{{Provider: "openai", Model: "gpt-4o"}},
		}},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	router := NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, nil, "test-version")

	// Real server so writes are actually flushed over a connection.
	gw := httptest.NewServer(router)
	defer gw.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"safe","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	select {
	case <-firstTokenSent:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the request")
	}

	lineCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		lineCh <- line
	}()

	select {
	case line := <-lineCh:
		t.Logf("first line = %q", strings.TrimSpace(line))
		if !strings.Contains(line, "one") {
			t.Fatalf("first delivered chunk = %q, want the first token", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no bytes delivered while upstream stream was still open (response was buffered)")
	}
}
