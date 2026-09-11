package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Server struct {
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
	Timeout string `yaml:"timeout,omitempty"`
}

func (s Server) ParsedTimeout() time.Duration {
	if s.Timeout == "" {
		return 30 * time.Second
	}
	d, err := time.ParseDuration(s.Timeout)
	if err != nil || d <= 0 {
		return 30 * time.Second
	}
	return d
}

type Provider struct {
	Name           string   `yaml:"name"`
	Type           string   `yaml:"type"`
	BaseURL        string   `yaml:"base_url"`
	APIKey         string   `yaml:"api_key,omitempty"`
	Models         []string `yaml:"models"`
	DisabledModels []string `yaml:"disabled_models,omitempty"`
	Disabled       bool     `yaml:"disabled,omitempty"`
	Timeout        string   `yaml:"timeout,omitempty"`
}

func (p Provider) ParsedTimeout(defaultTimeout time.Duration) time.Duration {
	if defaultTimeout <= 0 {
		defaultTimeout = 30 * time.Second
	}
	if p.Timeout == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(p.Timeout)
	if err != nil || d <= 0 {
		return defaultTimeout
	}
	return d
}

func (p Provider) IsModelDisabled(model string) bool {
	for _, m := range p.DisabledModels {
		if m == model {
			return true
		}
	}
	return false
}

type ComboTarget struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

type Combo struct {
	Name     string        `yaml:"name"`
	Strategy string        `yaml:"strategy"`
	Targets  []ComboTarget `yaml:"targets"`
	DrainTTL string        `yaml:"drain_ttl,omitempty"`
}

func (c Combo) ParsedDrainTTL() time.Duration {
	if c.DrainTTL == "" {
		return 60 * time.Second
	}
	d, err := time.ParseDuration(c.DrainTTL)
	if err != nil || d <= 0 {
		return 60 * time.Second
	}
	return d
}

type Config struct {
	Server    Server     `yaml:"server"`
	Providers []Provider `yaml:"providers"`
	Combos    []Combo    `yaml:"combos"`
	Timeout   string     `yaml:"timeout,omitempty"`
}

func (c *Config) DefaultTimeout() time.Duration {
	if c.Server.Timeout != "" {
		return c.Server.ParsedTimeout()
	}
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var validTypes = map[string]bool{"openai": true, "antigravity": true}
var validStrategies = map[string]bool{"priority": true, "fill-first": true}

func (c *Config) Validate() error {
	if c.Server.Timeout != "" {
		d, err := time.ParseDuration(c.Server.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("server: invalid timeout %q", c.Server.Timeout)
		}
	}
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("invalid timeout %q", c.Timeout)
		}
	}
	for _, p := range c.Providers {
		if !validTypes[p.Type] {
			return fmt.Errorf("provider %q: unknown type %q", p.Name, p.Type)
		}
		if p.Timeout != "" {
			d, err := time.ParseDuration(p.Timeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("provider %q: invalid timeout %q", p.Name, p.Timeout)
			}
		}
	}
	for _, cb := range c.Combos {
		if !validStrategies[cb.Strategy] {
			return fmt.Errorf("combo %q: unknown strategy %q", cb.Name, cb.Strategy)
		}
		if cb.DrainTTL != "" {
			d, err := time.ParseDuration(cb.DrainTTL)
			if err != nil || d <= 0 {
				return fmt.Errorf("combo %q: invalid drain_ttl %q", cb.Name, cb.DrainTTL)
			}
		}
	}
	return nil
}
