package opencode

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// A line longer than one read must not make the proxy retain every byte read
// before it: only the unterminated tail is kept. The tail still has to be
// forwarded whole and its usage still has to be counted.
func TestProxyStreamKeepsOnlyTheUnterminatedTail(t *testing.T) {
	big := strings.Repeat("x", 4<<20)
	feed := "data: {\"choices\":[{\"delta\":{\"content\":\"" + big + "\"}}]}\n" +
		`data: {"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}` + "\n"

	var got provider.TokenUsage
	ctx := provider.WithUsageSink(context.Background(), func(usage provider.TokenUsage) { got = usage })
	dst := httptest.NewRecorder()
	if err := proxyStream(ctx, strings.NewReader(feed), "acct", dst); err != nil {
		t.Fatalf("proxyStream: %v", err)
	}
	if dst.Body.String() != feed {
		t.Fatalf("forwarded %d bytes, want %d verbatim", dst.Body.Len(), len(feed))
	}
	if got.InputTokens != 3 || got.OutputTokens != 4 {
		t.Fatalf("usage = %+v, want 3 in / 4 out", got)
	}
}

// A stream that never completes a line must not grow the accumulator without
// bound: the scan gives up past the line cap the other providers use. The
// forwarding path is what this covers, so the real cap is used once.
func TestProxyStreamDropsAnOverlongUnterminatedLine(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a line at the real cap")
	}
	feed := bytes.Repeat([]byte("x"), provider.MaxSSELine+1024)
	dst := httptest.NewRecorder()
	if err := proxyStream(context.Background(), bytes.NewReader(feed), "", dst); err != nil {
		t.Fatalf("proxyStream: %v", err)
	}
	if dst.Body.Len() != len(feed) {
		t.Fatalf("forwarded %d bytes, want %d", dst.Body.Len(), len(feed))
	}
}

// Giving up on one overlong line must not blind the proxy for the rest of the
// stream: usage on later, normal frames still has to be counted.
func TestProxyStreamRecoversAfterAnOverlongLine(t *testing.T) {
	feed := bytes.Repeat([]byte("x"), provider.MaxSSELine+1024)
	feed = append(feed, '\n')
	feed = append(feed, []byte(`data: {"usage":{"prompt_tokens":5,"completion_tokens":6,"total_tokens":11}}`+"\n")...)

	var got provider.TokenUsage
	ctx := provider.WithUsageSink(context.Background(), func(usage provider.TokenUsage) { got = usage })
	dst := httptest.NewRecorder()
	if err := proxyStream(ctx, bytes.NewReader(feed), "acct", dst); err != nil {
		t.Fatalf("proxyStream: %v", err)
	}
	if got.InputTokens != 5 || got.OutputTokens != 6 {
		t.Fatalf("usage after the overlong line = %+v, want 5 in / 6 out", got)
	}
}

// A non-streaming body is the answer itself, so its cap is generous but still
// finite: a broken upstream must not decide this process's memory use.
func TestCopyJSONBodyWithUsageRejectsAnOversizedBody(t *testing.T) {
	huge := bytes.Repeat([]byte("x"), provider.MaxCompletionBodyBytes+1)
	var dst bytes.Buffer
	if err := copyJSONBodyWithUsage(context.Background(), bytes.NewReader(huge), "", &dst); err == nil {
		t.Fatal("oversized body was accepted")
	}
	if dst.Len() != 0 {
		t.Fatalf("wrote %d bytes of an oversized body", dst.Len())
	}
}

// collect drains a scanner in one shot, mirroring the proxy's use.
func collect(s *lineScanner, chunk []byte) []string {
	var out []string
	s.add(chunk, func(line []byte) { out = append(out, string(line)) })
	return out
}

// A single line that never terminates must not grow the accumulator without
// bound, and it must never be reported as a usage line. The limit is injected
// so the test does not allocate a real maximum-sized line.
func TestLineScannerDropsAnUnterminatedOverlongLine(t *testing.T) {
	s := lineScanner{limit: 64}
	if got := collect(&s, bytes.Repeat([]byte("data: {"), 32)); len(got) != 0 {
		t.Fatalf("overlong chunk produced %d lines", len(got))
	}
	if !s.discarding {
		t.Fatal("overlong line was not discarded")
	}
	if len(s.pending) != 0 {
		t.Fatalf("accumulator kept %d bytes of an overlong line", len(s.pending))
	}
	got := collect(&s, append([]byte("\n"), []byte("data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n")...))
	if len(got) != 1 || !strings.HasPrefix(got[0], "data:") {
		t.Fatalf("recovery produced %q", got)
	}
}

// The tail of an overlong line arriving in a later chunk is discarded too, and
// the line after it is scanned normally.
func TestLineScannerDiscardsRemainderAcrossChunks(t *testing.T) {
	s := lineScanner{limit: 32}
	if got := collect(&s, bytes.Repeat([]byte("x"), 33)); len(got) != 0 {
		t.Fatalf("overlong chunk produced %q", got)
	}
	if !s.discarding {
		t.Fatal("overlong line was not marked as discarded")
	}
	if got := collect(&s, []byte("still the same line\n")); len(got) != 0 {
		t.Fatalf("discarded tail produced %q", got)
	}
	if got := collect(&s, []byte("data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n")); len(got) != 1 {
		t.Fatalf("lines after the discarded tail = %q", got)
	}
}

// Lines are handed to the callback while they are still valid, so a chunk that
// both completes lines and leaves a tail cannot corrupt what was delivered.
func TestLineScannerDeliversLinesBeforeCompaction(t *testing.T) {
	var s lineScanner
	got := collect(&s, []byte("data: FIRST\ndata: PART"))
	if len(got) != 1 || got[0] != "data: FIRST" {
		t.Fatalf("lines = %q, want the completed line only", got)
	}
	if string(s.pending) != "data: PART" {
		t.Fatalf("tail = %q, want the unterminated remainder", s.pending)
	}
}

// Discarding an overlong line must also release the capacity it grew to, or a
// single hostile line would leave a large buffer pinned for the stream's life.
func TestLineScannerReleasesCapacityAfterDiscard(t *testing.T) {
	s := lineScanner{limit: 32}
	collect(&s, bytes.Repeat([]byte("x"), 4096))
	if !s.discarding {
		t.Fatal("overlong line was not discarded")
	}
	if cap(s.pending) != 0 {
		t.Fatalf("discarded line left a buffer of capacity %d", cap(s.pending))
	}
}
