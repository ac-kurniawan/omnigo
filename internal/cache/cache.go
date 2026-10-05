package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("cache: key not found")
	ErrExpired  = errors.New("cache: key expired")
)

type CacheBackend interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	Close() error
}

type item struct {
	val       []byte
	expiresAt time.Time
	key       string
}

type LRUCache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*item
	order    []string
}

func NewLRUCache(capacity int) *LRUCache {
	if capacity <= 0 {
		capacity = 1000
	}
	return &LRUCache{
		capacity: capacity,
		items:    make(map[string]*item),
		order:    make([]string, 0, capacity),
	}
}

func (c *LRUCache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	it, ok := c.items[key]
	if !ok {
		return nil, ErrNotFound
	}

	if time.Now().After(it.expiresAt) {
		delete(c.items, key)
		c.removeOrder(key)
		return nil, ErrExpired
	}

	// Move to back (most recently used)
	c.removeOrder(key)
	c.order = append(c.order, key)

	res := make([]byte, len(it.val))
	copy(res, it.val)
	return res, nil
}

func (c *LRUCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if it, ok := c.items[key]; ok {
		it.val = val
		it.expiresAt = time.Now().Add(ttl)
		c.removeOrder(key)
		c.order = append(c.order, key)
		return nil
	}

	for len(c.items) >= c.capacity && len(c.order) > 0 {
		oldest := c.order[0]
		delete(c.items, oldest)
		c.order = c.order[1:]
	}

	copied := make([]byte, len(val))
	copy(copied, val)
	c.items[key] = &item{
		val:       copied,
		expiresAt: time.Now().Add(ttl),
		key:       key,
	}
	c.order = append(c.order, key)
	return nil
}

func (c *LRUCache) Delete(ctx context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
	c.removeOrder(key)
	return nil
}

func (c *LRUCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*item)
	c.order = nil
	return nil
}

func (c *LRUCache) removeOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

type CanonicalKeyPayload struct {
	TenantID    string          `json:"t"`
	Model       string          `json:"m"`
	Messages    []NormalMessage `json:"msg"`
	Temperature float64         `json:"temp"`
}

type NormalMessage struct {
	Role    string `json:"r"`
	Content string `json:"c"`
}

func ComputeCacheKey(tenantID, model string, messages []NormalMessage, temp float64) string {
	payload := CanonicalKeyPayload{
		TenantID:    tenantID,
		Model:       strings.ToLower(strings.TrimSpace(model)),
		Messages:    messages,
		Temperature: temp,
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("omnigo:cache:%s:%s", tenantID, hex.EncodeToString(sum[:]))
}
