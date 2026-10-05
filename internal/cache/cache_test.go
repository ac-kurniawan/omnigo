package cache

import (
	"context"
	"testing"
	"time"
)

func TestLRUCacheOperations(t *testing.T) {
	c := NewLRUCache(2)
	ctx := context.Background()

	// 1. Set and Get
	err := c.Set(ctx, "k1", []byte("v1"), time.Minute)
	if err != nil {
		t.Fatalf("unexpected set error: %v", err)
	}

	val, err := c.Get(ctx, "k1")
	if err != nil || string(val) != "v1" {
		t.Fatalf("expected v1, got %s, err: %v", string(val), err)
	}

	// 2. Capacity eviction
	_ = c.Set(ctx, "k2", []byte("v2"), time.Minute)
	_ = c.Set(ctx, "k3", []byte("v3"), time.Minute)

	_, err = c.Get(ctx, "k1")
	if err != ErrNotFound {
		t.Fatalf("expected k1 to be evicted, got err: %v", err)
	}

	// 3. TTL expiration
	_ = c.Set(ctx, "k4", []byte("v4"), time.Millisecond*5)
	time.Sleep(time.Millisecond * 10)
	_, err = c.Get(ctx, "k4")
	if err != ErrExpired {
		t.Fatalf("expected k4 to be expired, got err: %v", err)
	}
}

func TestComputeCacheKey(t *testing.T) {
	msgs1 := []NormalMessage{{Role: "user", Content: "hello"}}
	msgs2 := []NormalMessage{{Role: "user", Content: "hello"}}

	key1 := ComputeCacheKey("tenantA", "gpt-4o", msgs1, 0.0)
	key2 := ComputeCacheKey("tenantA", "gpt-4o", msgs2, 0.0)
	keyTenantB := ComputeCacheKey("tenantB", "gpt-4o", msgs1, 0.0)

	if key1 != key2 {
		t.Fatalf("expected identical keys for identical payloads")
	}
	if key1 == keyTenantB {
		t.Fatalf("expected tenant isolation to produce different keys")
	}
}
