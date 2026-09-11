package combo

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrackerInMemory(t *testing.T) {
	tr := NewTracker("")
	target := Target{Provider: "openai", Model: "gpt-4o"}

	if tr.IsDrained(target) {
		t.Fatal("target should not be drained initially")
	}

	tr.MarkDrained(target, 50*time.Millisecond, "rate limit exceeded (429)")

	if !tr.IsDrained(target) {
		t.Fatal("target should be drained after MarkDrained")
	}

	if got := tr.DrainReason(target); got != "rate limit exceeded (429)" {
		t.Fatalf("DrainReason = %q, want 'rate limit exceeded (429)'", got)
	}

	remaining := tr.DrainRemaining(target)
	if remaining <= 0 || remaining > 50*time.Millisecond {
		t.Fatalf("unexpected drain remaining: %v", remaining)
	}

	time.Sleep(60 * time.Millisecond)

	if tr.IsDrained(target) {
		t.Fatal("target should have expired drain")
	}
	if got := tr.DrainReason(target); got != "" {
		t.Fatalf("expected empty reason after expiration, got %q", got)
	}
}

func TestTrackerClear(t *testing.T) {
	tr := NewTracker("")
	target := Target{Provider: "openai", Model: "gpt-4o"}

	tr.MarkDrained(target, time.Minute, "upstream 503")
	if !tr.IsDrained(target) {
		t.Fatal("expected drained")
	}

	tr.Clear(target)
	if tr.IsDrained(target) {
		t.Fatal("expected target to be cleared")
	}
	if got := tr.DrainReason(target); got != "" {
		t.Fatalf("expected empty reason after clear, got %q", got)
	}
}

func TestTrackerPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "drains.json")

	tr1 := NewTracker(path)
	t1 := Target{Provider: "p1", Model: "m1"}
	t2 := Target{Provider: "p2", Model: "m2"}

	tr1.MarkDrained(t1, 10*time.Second, "quota exceeded")
	tr1.MarkDrained(t2, 20*time.Second, "timeout after 30s")

	// Verify file was written
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected drains.json to exist: %v", err)
	}

	// Create new tracker pointing to same file
	tr2 := NewTracker(path)
	if !tr2.IsDrained(t1) {
		t.Fatalf("expected t1 to be drained after reload")
	}
	if tr2.DrainReason(t1) != "quota exceeded" {
		t.Fatalf("reason for t1 = %q, want 'quota exceeded'", tr2.DrainReason(t1))
	}
	if !tr2.IsDrained(t2) {
		t.Fatalf("expected t2 to be drained after reload")
	}
	if tr2.DrainReason(t2) != "timeout after 30s" {
		t.Fatalf("reason for t2 = %q, want 'timeout after 30s'", tr2.DrainReason(t2))
	}

	// Clearing in tr2 removes it and updates file
	tr2.Clear(t1)
	tr3 := NewTracker(path)
	if tr3.IsDrained(t1) {
		t.Fatalf("expected t1 to remain cleared after reload")
	}
	if !tr3.IsDrained(t2) {
		t.Fatalf("expected t2 to remain drained in tr3")
	}
}

func TestTrackerLogsDrainAndClear(t *testing.T) {
	var buf bytes.Buffer
	origWriter := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origWriter)

	tr := NewTracker("")
	t1 := Target{Provider: "openai", Model: "gpt-4o"}

	tr.MarkDrained(t1, 30*time.Second, "status 429: rate limited")
	if !strings.Contains(buf.String(), "[drained] openai/gpt-4o for 30s (reason: status 429: rate limited)") {
		t.Fatalf("log missing drain entry, got: %s", buf.String())
	}

	buf.Reset()
	tr.Clear(t1)
	if !strings.Contains(buf.String(), "[drained-cleared] openai/gpt-4o") {
		t.Fatalf("log missing clear entry, got: %s", buf.String())
	}
}
