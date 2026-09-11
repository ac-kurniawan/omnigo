package provider

import "sync"

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	factories[name] = f
}

func Get(name string) (Factory, bool) {
	mu.RLock()
	defer mu.RUnlock()
	f, ok := factories[name]
	return f, ok
}
