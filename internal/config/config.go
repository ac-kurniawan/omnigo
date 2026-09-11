package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Server struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type Provider struct {
	Name           string   `yaml:"name"`
	Type           string   `yaml:"type"`
	BaseURL        string   `yaml:"base_url"`
	APIKey         string   `yaml:"api_key,omitempty"`
	Models         []string `yaml:"models"`
	DisabledModels []string `yaml:"disabled_models,omitempty"`
	Disabled       bool     `yaml:"disabled,omitempty"`
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
}

type Config struct {
	Server    Server     `yaml:"server"`
	Providers []Provider `yaml:"providers"`
	Combos    []Combo    `yaml:"combos"`
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
	for _, p := range c.Providers {
		if !validTypes[p.Type] {
			return fmt.Errorf("provider %q: unknown type %q", p.Name, p.Type)
		}
	}
	for _, cb := range c.Combos {
		if !validStrategies[cb.Strategy] {
			return fmt.Errorf("combo %q: unknown strategy %q", cb.Name, cb.Strategy)
		}
	}
	return nil
}
