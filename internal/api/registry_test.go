package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
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

func BenchmarkBuildProviderPerRequest(b *testing.B) {
	provider.Register("bench-baseline", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{name: cfg.Name}
	})
	pc := config.Provider{Name: "bench", Type: "bench-baseline", BaseURL: "https://x"}
	store := vault.NewMemoryStore(&vault.Vault{})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := buildProvider(pc, store, nil, 30*time.Second); err != nil {
			b.Fatal(err)
		}
	}
}
