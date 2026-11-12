package commandcode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestChatStreamEndsWithDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"hi\"}\n{\"type\":\"finish\"}\n"))
	}))
	defer srv.Close()
	store := &memStore{accounts: []provider.Credentials{{APIKey: "k", AccountID: "u1"}}}
	p := New(provider.Config{Name: "cc", BaseURL: srv.URL}, store).(*Provider)
	w := httptest.NewRecorder()
	if err := p.ChatCompletion(context.Background(), chatReq(t, "m", true), w); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("stream missing [DONE] sentinel:\n%q", w.Body.String())
	}
}

func TestChatPropagatesDrainError(t *testing.T) {
	store := &memStore{accounts: []provider.Credentials{{APIKey: "k", AccountID: "u1"}}}
	p := New(provider.Config{Name: "cc"}, store).(*Provider)
	p.pool.MarkFailed(store.accounts[0], "m", provider.NewHTTPStatusError(http.StatusTooManyRequests, "commandcode: upstream status 429"))
	err := p.ChatCompletion(context.Background(), chatReq(t, "m", true), httptest.NewRecorder())
	if err == nil {
		t.Fatal("expected drain error")
	}
	if strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("drain masked as auth failure: %v", err)
	}
}

func TestTestProbe(t *testing.T) {
	whoami := `{"userId":"u","org":{"id":"o"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(whoami))
	}))
	defer srv.Close()
	store := &memStore{accounts: []provider.Credentials{{APIKey: "k", AccountID: "u"}}}
	p := New(provider.Config{Name: "cc", BaseURL: srv.URL}, store).(*Provider)
	res := p.Test(context.Background())
	if !res.OK {
		t.Fatalf("Test = %+v", res)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer bad.Close()
	p2 := New(provider.Config{Name: "cc", BaseURL: bad.URL}, store).(*Provider)
	res2 := p2.Test(context.Background())
	if res2.OK {
		t.Fatalf("Test should fail on 401: %+v", res2)
	}
	empty := New(provider.Config{Name: "cc"}, &memStore{}).(*Provider)
	if res3 := empty.Test(context.Background()); res3.OK {
		t.Fatalf("Test should fail without accounts: %+v", res3)
	}
}
