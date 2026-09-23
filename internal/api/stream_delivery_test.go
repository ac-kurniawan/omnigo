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
	"github.com/ac-kurniawan/omnigo/internal/observability"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

// A stream must reach the client chunk by chunk, not at upstream EOF. The
// gateway commits the response on the first flushed chunk and then forwards
// every later write, so the second chunk has to arrive while the upstream
// stream is still open. A buffered writer that stops flushing after the first
// commit, or a handler that buffers the whole response, fails here by timeout.
func TestStreamsEveryChunkBeforeUpstreamEOF(t *testing.T) {
	for _, strategy := range []string{"priority", "reliable", "fill-first"} {
		t.Run(strategy, func(t *testing.T) {
			// Register the real provider: sibling tests replace the global
			// "openai" factory with fakes, so this test must not depend on
			// registration order.
			provider.Register("openai", provider.NewOpenAI)
			secondTokenSent := make(chan struct{})
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n"))
				flusher.Flush()
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n"))
				flusher.Flush()
				close(secondTokenSent)
				<-release // hold the stream open: no further bytes yet
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				flusher.Flush()
			}))
			defer upstream.Close()
			defer close(release)

			cfg := &config.Config{
				Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: upstream.URL, Models: []string{"gpt-4o"}}},
				Combos: []config.Combo{{
					Name: "safe", Strategy: strategy,
					Targets: []config.ComboTarget{{Provider: "openai", Model: "gpt-4o"}},
				}},
			}
			raw, hash, prefix, _ := auth.GenerateKey()
			v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
			metrics, err := observability.New(func() bool { return true }, "test-version")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = metrics.Shutdown(context.Background()) }()
			router := NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, nil, "test-version", metrics)
			// Real server so writes are actually flushed over a connection; wrap the
			// router exactly as main does to prove metrics preserve SSE streaming.
			gw := httptest.NewServer(metrics.Middleware(router))
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
			case <-secondTokenSent:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream never received the request")
			}

			reader := bufio.NewReader(resp.Body)
			readUntil := func(want string, timeout time.Duration) {
				t.Helper()
				lines := make(chan string, 1)
				go func() {
					for {
						line, err := reader.ReadString('\n')
						if line != "" && strings.Contains(line, want) {
							lines <- line
							return
						}
						if err != nil {
							lines <- ""
							return
						}
					}
				}()
				select {
				case line := <-lines:
					if line == "" {
						t.Fatalf("stream ended before %q arrived", want)
					}
				case <-time.After(timeout):
					t.Fatalf("%q not delivered while the upstream stream was still open: response is buffered", want)
				}
			}
			readUntil("one", 3*time.Second)
			readUntil("two", 3*time.Second)
		})
	}
}

// A failure after the response has been committed must not be reported as a
// JSON error envelope: appending one to a delivered SSE body produces a frame
// clients parse as a malformed chunk.
func TestDirectPostCommitFailureDoesNotAppendJSONError(t *testing.T) {
	provider.Register("openai", provider.NewOpenAI)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n"))
		flusher.Flush()
		<-release
	}))
	defer upstream.Close()
	defer close(release)

	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: upstream.URL, Models: []string{"gpt-4o"}, Timeout: "200ms"}},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	router := NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, nil, "test-version", nil)
	gw := httptest.NewServer(router)
	defer gw.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"openai/gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 512)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if strings.Contains(string(buf), `"error"`) || err != nil {
			break
		}
	}
	body := string(buf)
	if !strings.Contains(body, "one") {
		t.Fatalf("first chunk missing: %q", body)
	}
	if strings.Contains(body, `"object":"error"`) || strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Fatalf("JSON error envelope appended to committed SSE body: %q", body)
	}
}

// Once a chunk is on the wire the gateway can no longer fail over, so a stream
// that dies afterwards must tell the client it failed. A bare close looks like
// a finished answer, which is what leaves callers retrying or inventing their
// own fallback text.
func TestPostCommitStreamFailureEmitsSSEError(t *testing.T) {
	provider.Register("openai", provider.NewOpenAI)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n"))
		flusher.Flush()
		<-release
	}))
	defer upstream.Close()
	defer close(release)

	cfg := &config.Config{
		Combos:    []config.Combo{{Name: "smart", Strategy: "fill-first", Targets: []config.ComboTarget{{Provider: "openai", Model: "gpt-4o"}}}},
		Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: upstream.URL, Models: []string{"gpt-4o"}, Timeout: "200ms"}},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	router := NewRouter(func() *config.Config { return cfg }, vault.NewMemoryStore(v), nil, nil, "test-version", nil)
	gw := httptest.NewServer(router)
	defer gw.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"smart","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 512)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, readErr := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if readErr != nil {
			break
		}
	}
	body := string(buf)
	if !strings.Contains(body, "one") {
		t.Fatalf("first chunk missing: %q", body)
	}
	if !strings.Contains(body, "event: error") {
		t.Fatalf("committed stream died without an SSE error frame: %q", body)
	}
}
