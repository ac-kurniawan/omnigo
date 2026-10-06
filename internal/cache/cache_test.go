package cache

import (
	"context"
	"encoding/json"
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

	key1, err := ComputeCacheKey("tenantA", "provider-a", "gpt-4o", msgs1, 0.0, 0)
	if err != nil {
		t.Fatalf("key1: %v", err)
	}
	key2, err := ComputeCacheKey("tenantA", "provider-a", "gpt-4o", msgs2, 0.0, 0)
	if err != nil {
		t.Fatalf("key2: %v", err)
	}
	keyTenantB, err := ComputeCacheKey("tenantB", "provider-a", "gpt-4o", msgs1, 0.0, 0)
	if err != nil {
		t.Fatalf("keyTenantB: %v", err)
	}
	keyProviderB, err := ComputeCacheKey("tenantA", "provider-b", "gpt-4o", msgs1, 0.0, 0)
	if err != nil {
		t.Fatalf("keyProviderB: %v", err)
	}
	keyCapped, err := ComputeCacheKey("tenantA", "provider-a", "gpt-4o", msgs1, 0.0, 16)
	if err != nil {
		t.Fatalf("keyCapped: %v", err)
	}
	keyOtherContent, err := ComputeCacheKey("tenantA", "provider-a", "gpt-4o", []NormalMessage{{Role: "user", Content: "other"}}, 0.0, 0)
	if err != nil {
		t.Fatalf("keyOtherContent: %v", err)
	}

	if key1 != key2 {
		t.Fatalf("expected identical keys for identical payloads")
	}
	if key1 == keyTenantB {
		t.Fatalf("expected tenant isolation to produce different keys")
	}
	if key1 == keyProviderB {
		t.Fatalf("expected provider isolation to produce different keys")
	}
	if key1 == keyCapped {
		t.Fatalf("expected different maxTokens to produce different keys")
	}
	if key1 == keyOtherContent {
		t.Fatalf("expected different message content to produce different keys")
	}
	largeA, err := ComputeCacheKey("tenantA", "provider-a", "gpt-4o", msgs1, 0, 9007199254740992)
	if err != nil {
		t.Fatalf("largeA: %v", err)
	}
	largeB, err := ComputeCacheKey("tenantA", "provider-a", "gpt-4o", msgs1, 0, 9007199254740993)
	if err != nil {
		t.Fatalf("largeB: %v", err)
	}
	if largeA == largeB {
		t.Fatalf("distinct maxTokens above 2^53 produced the same key")
	}
	zeroField, err := json.Marshal(CanonicalKeyPayload{TenantID: "tenantA", Model: "gpt-4o", Messages: msgs1})
	if err != nil {
		t.Fatalf("marshal zero payload: %v", err)
	}
	explicitZero, err := json.Marshal(CanonicalKeyPayload{TenantID: "tenantA", Model: "gpt-4o", Messages: msgs1, MaxTokens: 0})
	if err != nil {
		t.Fatalf("marshal explicit zero: %v", err)
	}
	if string(zeroField) != string(explicitZero) {
		t.Fatalf("maxTokens 0 must omit the field:\n zero=%s\n explicit=%s", zeroField, explicitZero)
	}
}
