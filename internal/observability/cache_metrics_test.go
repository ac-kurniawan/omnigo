package observability

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecordCacheMetrics(t *testing.T) {
	m, err := New(func() bool { return true }, "test")
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = m.Shutdown(context.Background()) }()

	ctx := context.Background()
	m.RecordCacheHit(ctx, "gpt-4o")
	m.RecordCacheMiss(ctx, "gpt-4o")

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body failed: %v", err)
	}

	out := string(body)
	if !strings.Contains(out, "omnigo_cache_hits") {
		t.Errorf("expected metric omnigo_cache_hits in scrape output, got:\n%s", out)
	}
	if !strings.Contains(out, "omnigo_cache_misses") {
		t.Errorf("expected metric omnigo_cache_misses in scrape output, got:\n%s", out)
	}
}
