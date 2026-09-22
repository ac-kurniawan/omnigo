package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Server struct {
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	Timeout       string `yaml:"timeout,omitempty"`
	StreamTimeout string `yaml:"stream_timeout,omitempty"`
}

func (s Server) ParsedTimeout() time.Duration {
	if s.Timeout == "" {
		return 20 * time.Second
	}
	d, err := time.ParseDuration(s.Timeout)
	if err != nil || d <= 0 {
		return 20 * time.Second
	}
	return d
}

// ParsedStreamTimeout returns the total wall-clock budget for one streamed
// generation. Unlike the request timeout, a zero budget is meaningful: it
// leaves streamed generations unbounded, so only silence is bounded.
func (s Server) ParsedStreamTimeout() time.Duration {
	return parseStreamTimeout(s.StreamTimeout, 10*time.Minute)
}

// parseStreamTimeout parses a stream budget where an unset value takes the
// fallback and a negative value is rejected as invalid by Validate.
func parseStreamTimeout(value string, fallback time.Duration) time.Duration {
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return fallback
	}
	return d
}

type Provider struct {
	Name           string   `yaml:"name"`
	Type           string   `yaml:"type"`
	BaseURL        string   `yaml:"base_url"`
	APIKey         string   `yaml:"-"`
	Models         []string `yaml:"models"`
	DisabledModels []string `yaml:"disabled_models,omitempty"`
	Disabled       bool     `yaml:"disabled,omitempty"`
	Timeout        string   `yaml:"timeout,omitempty"`
	StreamTimeout  string   `yaml:"stream_timeout,omitempty"`
	MaxConcurrency int      `yaml:"max_concurrency,omitempty"`
}

func (p Provider) ParsedTimeout(defaultTimeout time.Duration) time.Duration {
	if defaultTimeout <= 0 {
		defaultTimeout = 20 * time.Second
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

// ParsedStreamTimeout returns this provider's stream budget, falling back to
// the server-wide default when unset.
func (p Provider) ParsedStreamTimeout(defaultTimeout time.Duration) time.Duration {
	return parseStreamTimeout(p.StreamTimeout, defaultTimeout)
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

type Observability struct {
	// Metrics exposes Prometheus-format metrics at /actuator/metrics.
	// Default: disabled (nil).
	Metrics *bool `yaml:"metrics,omitempty"`
}

// MetricsEnabled reports whether the metrics endpoint and recording are on.
// Default: false (when Metrics is nil or explicitly false).
func (o Observability) MetricsEnabled() bool {
	return o.Metrics != nil && *o.Metrics
}

type Dashboard struct {
	// Enabled serves the dashboard UI and its /internal/* helpers. When false
	// the whole surface answers 404 and basic auth is not applied.
	// Default: true (when Enabled is nil or explicitly true).
	Enabled *bool `yaml:"enabled,omitempty"`
	Auth    *bool `yaml:"auth,omitempty"`
}

// IsEnabled returns whether the dashboard is served.
// Default: true (when Enabled is nil or explicitly true).
func (d Dashboard) IsEnabled() bool {
	return d.Enabled == nil || *d.Enabled
}

// AuthEnabled returns whether dashboard basic auth is on.
// Default: true (when Auth is nil or explicitly true).
func (d Dashboard) AuthEnabled() bool {
	return d.Auth == nil || *d.Auth
}

type Quota struct {
	// Enabled controls background polling and live quota visibility.
	// Default: true (when Enabled is nil or explicitly true).
	Enabled *bool `yaml:"enabled,omitempty"`
	// Interval is the polling frequency. Valid: >= 1m. Default: 5m.
	Interval string `yaml:"interval,omitempty"`
}

// IsEnabled returns whether quota collection and visibility are active.
// Default: true.
func (q Quota) IsEnabled() bool {
	return q.Enabled == nil || *q.Enabled
}

// ParsedInterval returns the configured interval, falling back to 5m.
func (q Quota) ParsedInterval() time.Duration {
	if q.Interval == "" {
		return 5 * time.Minute
	}
	d, err := time.ParseDuration(q.Interval)
	if err != nil || d < time.Minute {
		return 5 * time.Minute
	}
	return d
}

type Config struct {
	Server        Server        `yaml:"server"`
	Dashboard     Dashboard     `yaml:"dashboard,omitempty"`
	Observability Observability `yaml:"observability,omitempty"`
	Quota         Quota         `yaml:"quota,omitempty"`
	Providers     []Provider    `yaml:"providers"`
	Combos        []Combo       `yaml:"combos"`
	Timeout       string        `yaml:"timeout,omitempty"`
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
	return 20 * time.Second
}

// DefaultStreamTimeout returns the server-wide stream budget: the total
// wall-clock time one streamed generation may run before the gateway gives up
// on the upstream. Zero leaves streams unbounded.
func (c *Config) DefaultStreamTimeout() time.Duration {
	return c.Server.ParsedStreamTimeout()
}

// Timeouts are the server-wide defaults a provider inherits.
type Timeouts struct {
	Request time.Duration
	Stream  time.Duration
}

// Timeouts resolves the configured server defaults.
func (c *Config) Timeouts() Timeouts {
	return Timeouts{Request: c.DefaultTimeout(), Stream: c.DefaultStreamTimeout()}
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

var validTypes = map[string]bool{"openai": true, "antigravity": true, "codex": true}
var validStrategies = map[string]bool{"priority": true, "fill-first": true, "reliable": true, "round-robin": true}

func (c *Config) Validate() error {
	if c.Server.StreamTimeout != "" {
		if err := validateStreamTimeout(c.Server.StreamTimeout); err != nil {
			return fmt.Errorf("server: %w", err)
		}
	}
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
		if p.StreamTimeout != "" {
			if err := validateStreamTimeout(p.StreamTimeout); err != nil {
				return fmt.Errorf("provider %q: %w", p.Name, err)
			}
		}
		if p.MaxConcurrency < 0 {
			return fmt.Errorf("provider %q: invalid max_concurrency %d", p.Name, p.MaxConcurrency)
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
	if c.Quota.Interval != "" {
		d, err := time.ParseDuration(c.Quota.Interval)
		if err != nil {
			return fmt.Errorf("quota: invalid interval %q: %w", c.Quota.Interval, err)
		}
		if d < time.Minute {
			return fmt.Errorf("quota: interval %q below minimum 1m", c.Quota.Interval)
		}
	}

	return nil
}

// validateStreamTimeout accepts any non-negative duration: zero is the explicit
// opt-out from bounding a streamed generation.
func validateStreamTimeout(value string) error {
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return fmt.Errorf("invalid stream_timeout %q", value)
	}
	return nil
}
