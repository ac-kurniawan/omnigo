package combo

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Tracker maintains exhaustion/drain state for targets.
// It keeps state in RAM for lock-free-like fast reads and flushes to a JSON file
// on mutations if a path is configured.
type Tracker struct {
	mu     sync.RWMutex
	path   string
	drains map[string]time.Time // key: "provider/model" -> drain until
}

func NewTracker(path string) *Tracker {
	t := &Tracker{
		path:   path,
		drains: make(map[string]time.Time),
	}
	if path != "" {
		t.load()
	}
	return t
}

func targetKey(t Target) string {
	return t.Provider + "/" + t.Model
}

func (t *Tracker) IsDrained(target Target) bool {
	t.mu.RLock()
	until, ok := t.drains[targetKey(target)]
	t.mu.RUnlock()
	if !ok {
		return false
	}
	return time.Now().Before(until)
}

func (t *Tracker) DrainRemaining(target Target) time.Duration {
	t.mu.RLock()
	until, ok := t.drains[targetKey(target)]
	t.mu.RUnlock()
	if !ok {
		return 0
	}
	rem := time.Until(until)
	if rem < 0 {
		return 0
	}
	return rem
}

func (t *Tracker) MarkDrained(target Target, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	t.mu.Lock()
	t.drains[targetKey(target)] = time.Now().Add(ttl)
	t.mu.Unlock()
	t.persist()
}

func (t *Tracker) Clear(target Target) {
	t.mu.Lock()
	delete(t.drains, targetKey(target))
	t.mu.Unlock()
	t.persist()
}

func (t *Tracker) load() {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var stored map[string]time.Time
	if err := json.Unmarshal(b, &stored); err != nil {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, until := range stored {
		if until.After(now) {
			t.drains[k] = until
		}
	}
}

func (t *Tracker) persist() {
	if t.path == "" {
		return
	}
	t.mu.RLock()
	active := make(map[string]time.Time)
	now := time.Now()
	for k, until := range t.drains {
		if until.After(now) {
			active[k] = until
		}
	}
	t.mu.RUnlock()

	b, err := json.MarshalIndent(active, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(t.path, b, 0644)
}
