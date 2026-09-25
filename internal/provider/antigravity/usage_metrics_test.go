package antigravity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestAntigravityReportsUsageOnCompletedResponse(t *testing.T) {
	const frame = "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":2,\"totalTokenCount\":10}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(frame))
	}))
	defer srv.Close()

	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, staticStore{provider.Credentials{
		AccessToken: "tok", AccountID: "google-1", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour),
	}})
	for _, stream := range []bool{false, true} {
		var got []provider.TokenUsage
		ctx := provider.WithUsageSink(context.Background(), func(usage provider.TokenUsage) {
			got = append(got, usage)
		})
		err := p.ChatCompletion(ctx, provider.ChatRequest{
			Model: "gemini", Stream: stream, Messages: []provider.Message{{Role: "user", Content: "hi"}},
		}, httptest.NewRecorder())
		if err != nil {
			t.Fatalf("stream=%v: %v", stream, err)
		}
		if len(got) != 1 || got[0].InputTokens != 8 || got[0].OutputTokens != 2 || got[0].Account != "google-1" || !got[0].Present {
			t.Fatalf("stream=%v usage = %+v", stream, got)
		}
		if got[0].CachedTokens != 0 || got[0].ReasoningTokens != 0 || got[0].CacheCreationInputTokens != 0 {
			t.Fatalf("stream=%v invented a split the upstream did not send: %+v", stream, got[0])
		}
	}
}

func TestAntigravityDropsUsageWhenResponseOmitsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer srv.Close()
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, staticStore{provider.Credentials{
		AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour),
	}})
	var got int
	ctx := provider.WithUsageSink(context.Background(), func(provider.TokenUsage) { got++ })
	if err := p.ChatCompletion(ctx, provider.ChatRequest{
		Model: "gemini", Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, httptest.NewRecorder()); err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("samples = %d, want none when usageMetadata is absent", got)
	}
}

func TestAntigravityDoesNotReportUsageOnIncompleteStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"cut\"}]}}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":2,\"totalTokenCount\":10}}\n\n"))
	}))
	defer srv.Close()
	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, staticStore{provider.Credentials{
		AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour),
	}})
	var got int
	ctx := provider.WithUsageSink(context.Background(), func(provider.TokenUsage) { got++ })
	err := p.ChatCompletion(ctx, provider.ChatRequest{
		Model: "gemini", Stream: true, Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, httptest.NewRecorder())
	if err == nil {
		t.Fatal("incomplete stream returned success")
	}
	if got != 0 {
		t.Fatalf("samples = %d, want none for an incomplete generation", got)
	}
}
