package combo

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

type drainEntry struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}

// Tracker maintains exhaustion/drain state for targets.
// It keeps state in RAM for lock-free-like fast reads and flushes to a JSON file
// on mutations if a path is configured.
type Tracker struct {
	mu        sync.RWMutex
	path      string
	drains    map[string]drainEntry // key: "provider/model" -> entry
	save      chan struct{}
	flush     chan chan struct{}
	writeFile func(string, []byte, os.FileMode) error
}

func NewTracker(path string) *Tracker {
	t := &Tracker{
		path:      path,
		drains:    make(map[string]drainEntry),
		writeFile: os.WriteFile,
	}
	if path != "" {
		t.load()
		t.save = make(chan struct{}, 1)
		t.flush = make(chan chan struct{})
		go t.persistLoop()
	}
	return t
}

func targetKey(t Target) string {
	return t.Provider + "/" + t.Model
}

func (t *Tracker) IsDrained(target Target) bool {
	t.mu.RLock()
	entry, ok := t.drains[targetKey(target)]
	t.mu.RUnlock()
	if !ok {
		return false
	}
	return time.Now().Before(entry.Until)
}

func (t *Tracker) DrainRemaining(target Target) time.Duration {
	t.mu.RLock()
	entry, ok := t.drains[targetKey(target)]
	t.mu.RUnlock()
	if !ok {
		return 0
	}
	rem := time.Until(entry.Until)
	if rem < 0 {
		return 0
	}
	return rem
}

func (t *Tracker) DrainReason(target Target) string {
	t.mu.RLock()
	entry, ok := t.drains[targetKey(target)]
	t.mu.RUnlock()
	if !ok || !time.Now().Before(entry.Until) {
		return ""
	}
	return entry.Reason
}

func (t *Tracker) MarkDrained(target Target, ttl time.Duration, reason string) {
	if ttl <= 0 {
		return
	}
	key := targetKey(target)
	until := time.Now().Add(ttl)
	t.mu.Lock()
	t.drains[key] = drainEntry{Until: until, Reason: reason}
	t.mu.Unlock()
	log.Printf("[drained] %s for %v (reason: %s)", key, ttl.Round(time.Second), reason)
	t.schedulePersist()
}

func (t *Tracker) Clear(target Target) {
	key := targetKey(target)
	t.mu.Lock()
	delete(t.drains, key)
	t.mu.Unlock()
	log.Printf("[drained-cleared] %s", key)
	t.schedulePersist()
}

func (t *Tracker) ClearAll() {
	t.mu.Lock()
	count := len(t.drains)
	t.drains = make(map[string]drainEntry)
	t.mu.Unlock()
	log.Printf("[drained-cleared-all] cleared %d targets", count)
	t.schedulePersist()
}

func (t *Tracker) DrainedCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	now := time.Now()
	count := 0
	for _, entry := range t.drains {
		if now.Before(entry.Until) {
			count++
		}
	}
	return count
}

func (t *Tracker) load() {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	// First try new format map[string]drainEntry
	var stored map[string]drainEntry
	if err := json.Unmarshal(b, &stored); err == nil && len(stored) > 0 {
		now := time.Now()
		t.mu.Lock()
		defer t.mu.Unlock()
		for k, entry := range stored {
			if entry.Until.After(now) {
				t.drains[k] = entry
			}
		}
		return
	}
	// Fallback to legacy map[string]time.Time
	var legacy map[string]time.Time
	if err := json.Unmarshal(b, &legacy); err == nil {
		now := time.Now()
		t.mu.Lock()
		defer t.mu.Unlock()
		for k, until := range legacy {
			if until.After(now) {
				t.drains[k] = drainEntry{Until: until, Reason: ""}
			}
		}
	}
}

func (t *Tracker) schedulePersist() {
	if t.save == nil {
		return
	}
	select {
	case t.save <- struct{}{}:
	default:
	}
}

func (t *Tracker) Flush() {
	if t.flush == nil {
		return
	}
	done := make(chan struct{})
	t.flush <- done
	<-done
}

func (t *Tracker) persistLoop() {
	for {
		select {
		case <-t.save:
			t.persist()
		case done := <-t.flush:
			select {
			case <-t.save:
			default:
			}
			t.persist()
			close(done)
		}
	}
}

func (t *Tracker) persist() {
	t.mu.RLock()
	active := make(map[string]drainEntry)
	now := time.Now()
	for k, entry := range t.drains {
		if entry.Until.After(now) {
			active[k] = entry
		}
	}
	t.mu.RUnlock()

	b, err := json.MarshalIndent(active, "", "  ")
	if err != nil {
		return
	}
	if err := os.Chmod(t.path, 0o600); err != nil && !os.IsNotExist(err) {
		return
	}
	_ = t.writeFile(t.path, b, 0o600)
}
