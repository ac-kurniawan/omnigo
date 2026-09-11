package combo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTrackerInMemory(t *testing.T) {
	tr := NewTracker("")
	target := Target{Provider: "openai", Model: "gpt-4o"}

	if tr.IsDrained(target) {
		t.Fatal("target should not be drained initially")
	}

	tr.MarkDrained(target, 50*time.Millisecond)

	if !tr.IsDrained(target) {
		t.Fatal("target should be drained after MarkDrained")
	}

	remaining := tr.DrainRemaining(target)
	if remaining <= 0 || remaining > 50*time.Millisecond {
		t.Fatalf("unexpected drain remaining: %v", remaining)
	}

	time.Sleep(60 * time.Millisecond)

	if tr.IsDrained(target) {
		t.Fatal("target should have expired drain")
	}
}

func TestTrackerClear(t *testing.T) {
	tr := NewTracker("")
	target := Target{Provider: "openai", Model: "gpt-4o"}

	tr.MarkDrained(target, time.Minute)
	if !tr.IsDrained(target) {
		t.Fatal("expected drained")
	}

	tr.Clear(target)
	if tr.IsDrained(target) {
		t.Fatal("expected target to be cleared")
	}
}

func TestTrackerPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "drains.json")

	tr1 := NewTracker(path)
	t1 := Target{Provider: "p1", Model: "m1"}
	t2 := Target{Provider: "p2", Model: "m2"}

	tr1.MarkDrained(t1, 10*time.Second)
	tr1.MarkDrained(t2, 20*time.Second)

	// Verify file was written
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected drains.json to exist: %v", err)
	}

	// Create new tracker pointing to same file
	tr2 := NewTracker(path)
	if !tr2.IsDrained(t1) {
		t.Fatalf("expected t1 to be drained after reload")
	}
	if !tr2.IsDrained(t2) {
		t.Fatalf("expected t2 to be drained after reload")
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
