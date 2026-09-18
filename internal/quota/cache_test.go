package quota

import (
	"sync"
	"testing"
	"time"
)

func TestCachePutGetByProviderAndIdentity(t *testing.T) {
	c := NewCache()
	snap := AccountSnapshot{Provider: "cx", Identity: "acct-1", Status: StatusAvailable, ObservedAt: time.Unix(10, 0)}
	c.Put(snap)

	got, ok := c.Get("cx", "acct-1")
	if !ok {
		t.Fatal("snapshot not found")
	}
	if got.Identity != "acct-1" || got.Status != StatusAvailable {
		t.Fatalf("got %+v", got)
	}
	if _, ok := c.Get("cx", "other"); ok {
		t.Fatal("unexpected snapshot for unknown identity")
	}
	if _, ok := c.Get("agy", "acct-1"); ok {
		t.Fatal("snapshot leaked across providers")
	}
}

func TestCachePutReplacesPriorSnapshot(t *testing.T) {
	c := NewCache()
	c.Put(AccountSnapshot{Provider: "cx", Identity: "a", Status: StatusAvailable})
	c.Put(AccountSnapshot{Provider: "cx", Identity: "a", Status: StatusExhausted})

	got, _ := c.Get("cx", "a")
	if got.Status != StatusExhausted {
		t.Fatalf("status = %q, want exhausted", got.Status)
	}
	if n := len(c.All()); n != 1 {
		t.Fatalf("All() len = %d, want 1", n)
	}
}

func TestCacheAllReturnsSnapshotCopy(t *testing.T) {
	c := NewCache()
	c.Put(AccountSnapshot{Provider: "cx", Identity: "a"})

	all := c.All()
	all[0].Status = StatusExhausted
	if got, _ := c.Get("cx", "a"); got.Status == StatusExhausted {
		t.Fatal("All() exposed the internal snapshot to mutation")
	}
}

func TestCacheEmptyKeyIgnored(t *testing.T) {
	c := NewCache()
	c.Put(AccountSnapshot{Provider: "cx"})
	if n := len(c.All()); n != 0 {
		t.Fatalf("All() len = %d, want 0", n)
	}
}

func TestCacheConcurrentAccess(t *testing.T) {
	c := NewCache()
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.Put(AccountSnapshot{Provider: "cx", Identity: "a", Status: StatusAvailable, Reason: string(rune('a' + i%26))})
		}()
		go func() { defer wg.Done(); c.Get("cx", "a"); c.All() }()
	}
	wg.Wait()
	if _, ok := c.Get("cx", "a"); !ok {
		t.Fatal("snapshot missing after concurrent writes")
	}
}

func TestSnapshotKeyIsProviderQualified(t *testing.T) {
	c := NewCache()
	c.Put(AccountSnapshot{Provider: "agy", Identity: "shared"})
	c.Put(AccountSnapshot{Provider: "cx", Identity: "shared"})
	if n := len(c.All()); n != 2 {
		t.Fatalf("All() len = %d, want 2 (same identity under two providers)", n)
	}
}
