package config

import (
	"fmt"
	"time"
)

type CacheConfig struct {
	Enabled  *bool  `yaml:"enabled"`
	Backend  string `yaml:"backend"`
	TTL      string `yaml:"ttl"`
	MaxItems int    `yaml:"max_items"`
	RedisURL string `yaml:"redis_url"`
}

func (c *CacheConfig) EnabledOrDefault() bool {
	if c == nil || c.Enabled == nil {
		return false
	}
	return *c.Enabled
}

func (c *CacheConfig) BackendOrDefault() string {
	if c == nil || c.Backend == "" {
		return "memory"
	}
	return c.Backend
}

func (c *CacheConfig) ParsedTTL() time.Duration {
	if c == nil || c.TTL == "" {
		return time.Hour
	}
	d, err := time.ParseDuration(c.TTL)
	if err != nil {
		return time.Hour
	}
	return d
}

func (c *CacheConfig) MaxItemsOrDefault() int {
	if c == nil || c.MaxItems <= 0 {
		return 1000
	}
	return c.MaxItems
}

func (c *CacheConfig) Validate() error {
	if !c.EnabledOrDefault() {
		return nil
	}
	if c.TTL != "" {
		if _, err := time.ParseDuration(c.TTL); err != nil {
			return fmt.Errorf("invalid cache ttl %q: %w", c.TTL, err)
		}
	}
	switch c.BackendOrDefault() {
	case "memory":
	case "redis":
		if c.RedisURL == "" {
			return fmt.Errorf("redis_url is required when cache backend is redis")
		}
	default:
		return fmt.Errorf("unsupported cache backend %q", c.BackendOrDefault())
	}
	return nil
}
