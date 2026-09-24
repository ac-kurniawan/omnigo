package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/provider/antigravity"
	"github.com/ac-kurniawan/omnigo/internal/provider/codex"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestSharedTransportReusesConnections(t *testing.T) {
	provider.Register("openai-pool-test", provider.NewOpenAI)
	var conns atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	upstream.Start()
	defer upstream.Close()

	cfg := &config.Config{
		Providers: []config.Provider{{Name: "pool", Type: "openai-pool-test", BaseURL: upstream.URL, Models: []string{"gpt-4o"}}},
	}
	raw, hash, prefix, _ := auth.GenerateKey()
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	router := testRouter(t, cfg, v)

	for i := range 3 {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"pool/gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+raw)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d status = %d body = %s", i+1, rr.Code, rr.Body.String())
		}
	}
	if got := conns.Load(); got != 1 {
		t.Fatalf("upstream TCP connections = %d, want 1 (keep-alive reuse)", got)
	}
}

// A provider configured with a long timeout must be allowed to wait for
// response headers that long: a hardcoded transport cap would undercut it for
// every provider sharing the pool, since the pool is shared and the cap cannot
// vary per provider.
func TestSharedTransportDoesNotCapTimeToFirstByte(t *testing.T) {
	if got := newSharedTransport().ResponseHeaderTimeout; got != 0 {
		t.Fatalf("ResponseHeaderTimeout = %v, want 0 (bounded per request by the configured timeout)", got)
	}
}

func TestRegistryKeepsDisabledProvidersForInternalEndpoints(t *testing.T) {
	provider.Register("disabled-registry-test", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name}
	})
	registry := newProviderRegistry(vault.NewMemoryStore(&vault.Vault{}))
	cfg := &config.Config{Providers: []config.Provider{{Name: "off", Type: "disabled-registry-test", Disabled: true}}}
	registry.ensure(cfg)
	if _, ok := registry.Get(cfg, "off"); !ok {
		t.Fatal("disabled provider should still be cached for dashboard test/refresh endpoints")
	}
}

func TestRegistryReusesCodexProviderAndCredentialsAcrossReload(t *testing.T) {
	var builds atomic.Int32
	provider.Register("codex-reload-test", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		builds.Add(1)
		return &fakeProvider{name: cfg.Name}
	})
	store := vault.NewMemoryStore(&vault.Vault{ProviderSecrets: map[string]vault.ProviderSecret{
		"codex-main": {AccessToken: "access", RefreshToken: "refresh", AccountID: "account"},
	}})
	registry := newProviderRegistry(store)
	cfg := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex-reload-test", Models: []string{"gpt-old"}}}}
	first, ok := registry.Get(cfg, "codex-main")
	if !ok {
		t.Fatal("Codex provider missing")
	}
	cfg2 := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex-reload-test", Models: []string{"gpt-new"}}}}
	second, ok := registry.Get(cfg2, "codex-main")
	if !ok || first != second || builds.Load() != 1 {
		t.Fatalf("provider reuse = %v, builds = %d", first == second, builds.Load())
	}
	secret := store.Get().Accounts("codex-main")[0]
	if secret.RefreshToken != "refresh" || secret.AccountID != "account" {
		t.Fatalf("credentials changed across reload: %+v", secret)
	}
}

// A changed stream timeout must rebuild the provider: reusing the cached
// instance would silently keep imposing the previous budget after a reload.
func TestRegistryRebuildsProviderOnStreamTimeoutChange(t *testing.T) {
	var got provider.Config
	provider.Register("stream-timeout-reload-test", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		got = cfg
		return &fakeProvider{name: cfg.Name}
	})
	registry := newProviderRegistry(vault.NewMemoryStore(&vault.Vault{}))
	cfg := &config.Config{Providers: []config.Provider{{Name: "p", Type: "stream-timeout-reload-test", StreamTimeout: "1m"}}}
	if _, ok := registry.Get(cfg, "p"); !ok {
		t.Fatal("provider missing")
	}
	if got.StreamTimeout != time.Minute {
		t.Fatalf("stream timeout = %v, want 1m", got.StreamTimeout)
	}

	cfg2 := &config.Config{Providers: []config.Provider{{Name: "p", Type: "stream-timeout-reload-test", StreamTimeout: "9m"}}}
	if _, ok := registry.Get(cfg2, "p"); !ok {
		t.Fatal("provider missing after reload")
	}
	if got.StreamTimeout != 9*time.Minute {
		t.Fatalf("stream timeout after reload = %v, want 9m (provider was reused)", got.StreamTimeout)
	}
}

// Token refresh and quota polls must ride the registry pool, not a private
// default transport, while keeping their own 15s client so they never inherit
// the streaming client's unbounded deadline.
func TestUnaryClientsShareRegistryTransport(t *testing.T) {
	agyTransport := antigravity.UnaryClient().Transport
	cxTransport := codex.UnaryClient().Transport
	t.Cleanup(func() {
		antigravity.SetHTTPTransport(agyTransport)
		codex.SetHTTPTransport(cxTransport)
	})

	registry := newProviderRegistry(vault.NewMemoryStore(&vault.Vault{}))
	cfg := &config.Config{Providers: []config.Provider{
		{Name: "agy", Type: "antigravity"},
		{Name: "cx", Type: "codex"},
	}}
	registry.ensure(cfg)

	agy, ok := registry.Get(cfg, "agy")
	if !ok {
		t.Fatal("antigravity provider missing")
	}
	cx, ok := registry.Get(cfg, "cx")
	if !ok {
		t.Fatal("codex provider missing")
	}

	agyUnary := antigravity.UnaryClient()
	cxUnary := codex.UnaryClient()
	if agyUnary.Timeout != 15*time.Second || cxUnary.Timeout != 15*time.Second {
		t.Fatalf("unary timeouts = %v, %v; want 15s", agyUnary.Timeout, cxUnary.Timeout)
	}
	if agyUnary.Transport != registry.transport || cxUnary.Transport != registry.transport {
		t.Fatalf("unary transports = %T, %T; want the registry transport", agyUnary.Transport, cxUnary.Transport)
	}

	agyStream, ok := providerClient(agy)
	if !ok || agyStream == agyUnary {
		t.Fatal("antigravity streaming client must be distinct from the unary client")
	}
	cxStream, ok := providerClient(cx)
	if !ok || cxStream == cxUnary {
		t.Fatal("codex streaming client must be distinct from the unary client")
	}
	if agyStream.Timeout != 0 || cxStream.Timeout != 0 {
		t.Fatalf("streaming timeouts = %v, %v; want unbounded", agyStream.Timeout, cxStream.Timeout)
	}
}

func providerClient(p provider.Provider) (*http.Client, bool) {
	type clientHolder interface{ Client() *http.Client }
	holder, ok := p.(clientHolder)
	if !ok {
		return nil, false
	}
	return holder.Client(), true
}

// The bounded clients no longer own a transport, so the registry's one close
// must reach the connections their token and quota calls parked.
func TestCloseIdleConnectionsReachesUnaryClients(t *testing.T) {
	agyTransport := antigravity.UnaryClient().Transport
	t.Cleanup(func() {
		antigravity.SetHTTPTransport(agyTransport)
	})
	var mu sync.Mutex
	open := map[net.Conn]bool{}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	upstream.Config.ConnState = func(c net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch state {
		case http.StateNew:
			open[c] = true
		case http.StateClosed:
			delete(open, c)
		}
	}
	upstream.Start()
	defer upstream.Close()

	registry := newProviderRegistry(vault.NewMemoryStore(&vault.Vault{}))
	cfg := &config.Config{Providers: []config.Provider{{Name: "agy", Type: "antigravity"}}}
	registry.ensure(cfg)

	saved := antigravity.BaseURL()
	antigravity.SetBaseURL(upstream.URL)
	t.Cleanup(func() { antigravity.SetBaseURL(saved) })

	resp, err := antigravity.UnaryClient().Get(upstream.URL + "/v1internal:fetchAvailableModels")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if countOpen(open, &mu) == 0 {
		t.Fatal("unary call opened no pooled connection")
	}

	registry.CloseIdleConnections()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && countOpen(open, &mu) != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := countOpen(open, &mu); got != 0 {
		t.Fatalf("idle connections after close = %d, want 0", got)
	}
}

func countOpen(open map[net.Conn]bool, mu *sync.Mutex) int {
	mu.Lock()
	defer mu.Unlock()
	return len(open)
}

func BenchmarkBuildProviderPerRequest(b *testing.B) {
	provider.Register("bench-baseline", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name}
	})
	pc := config.Provider{Name: "bench", Type: "bench-baseline", BaseURL: "https://x"}
	store := vault.NewMemoryStore(&vault.Vault{})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := buildProvider(pc, store, nil, config.Timeouts{Request: 20 * time.Second, Stream: 10 * time.Minute}); err != nil {
			b.Fatal(err)
		}
	}
}
