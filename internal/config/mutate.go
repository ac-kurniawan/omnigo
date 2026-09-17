package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Mutate reads config.yaml, applies fn, validates the result, and writes it
// back. Used for model-list caching and provider/combo management.
func Mutate(path string, fn func(*Config) error) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if err := fn(&cfg); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// MutateFunc applies a change to the live config (read-modify-write + reload).
// Callers inject it; main wires it to config.Mutate(path, fn) plus a reload.
type MutateFunc func(fn func(*Config) error) error

// SetModels updates a provider's cached model list within an in-memory Config,
// filtering out any models that are currently in DisabledModels.
func SetModels(c *Config, providerName string, models []string) error {
	for i := range c.Providers {
		if c.Providers[i].Name == providerName {
			var active []string
			for _, m := range models {
				if !c.Providers[i].IsModelDisabled(m) {
					active = append(active, m)
				}
			}
			c.Providers[i].Models = active
			return nil
		}
	}
	return fmt.Errorf("provider %q not found", providerName)
}

// UpdateModels sets a provider's cached model list on disk.
func UpdateModels(path, providerName string, models []string) error {
	return Mutate(path, func(c *Config) error {
		return SetModels(c, providerName, models)
	})
}

// AddModel adds a model to a provider's active models list.
// If the model was previously in DisabledModels, it is removed from DisabledModels.
func AddModel(path, providerName, modelID string) error {
	return Mutate(path, func(c *Config) error {
		for i := range c.Providers {
			if c.Providers[i].Name == providerName {
				p := &c.Providers[i]
				var newDisabled []string
				for _, m := range p.DisabledModels {
					if m != modelID {
						newDisabled = append(newDisabled, m)
					}
				}
				p.DisabledModels = newDisabled

				for _, m := range p.Models {
					if m == modelID {
						return nil
					}
				}
				p.Models = append(p.Models, modelID)
				return nil
			}
		}
		return fmt.Errorf("provider %q not found", providerName)
	})
}

// DisableModel moves a model from active Models to DisabledModels.
func DisableModel(path, providerName, modelID string) error {
	return Mutate(path, func(c *Config) error {
		for i := range c.Providers {
			if c.Providers[i].Name == providerName {
				p := &c.Providers[i]
				var newActive []string
				for _, m := range p.Models {
					if m != modelID {
						newActive = append(newActive, m)
					}
				}
				p.Models = newActive

				for _, m := range p.DisabledModels {
					if m == modelID {
						return nil
					}
				}
				p.DisabledModels = append(p.DisabledModels, modelID)
				return nil
			}
		}
		return fmt.Errorf("provider %q not found", providerName)
	})
}

// EnableModel moves a model from DisabledModels back to active Models.
func EnableModel(path, providerName, modelID string) error {
	return Mutate(path, func(c *Config) error {
		for i := range c.Providers {
			if c.Providers[i].Name == providerName {
				p := &c.Providers[i]
				var newDisabled []string
				for _, m := range p.DisabledModels {
					if m != modelID {
						newDisabled = append(newDisabled, m)
					}
				}
				p.DisabledModels = newDisabled

				for _, m := range p.Models {
					if m == modelID {
						return nil
					}
				}
				p.Models = append(p.Models, modelID)
				return nil
			}
		}
		return fmt.Errorf("provider %q not found", providerName)
	})
}

// DeleteModel removes a model from both Models and DisabledModels.
func DeleteModel(path, providerName, modelID string) error {
	return Mutate(path, func(c *Config) error {
		for i := range c.Providers {
			if c.Providers[i].Name == providerName {
				p := &c.Providers[i]
				var newActive []string
				for _, m := range p.Models {
					if m != modelID {
						newActive = append(newActive, m)
					}
				}
				p.Models = newActive

				var newDisabled []string
				for _, m := range p.DisabledModels {
					if m != modelID {
						newDisabled = append(newDisabled, m)
					}
				}
				p.DisabledModels = newDisabled
				return nil
			}
		}
		return fmt.Errorf("provider %q not found", providerName)
	})
}

// SetProviderDisabled sets a provider's Disabled flag in-memory.
func SetProviderDisabled(c *Config, name string, disabled bool) error {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			c.Providers[i].Disabled = disabled
			return nil
		}
	}
	return fmt.Errorf("provider %q not found", name)
}

// SetCombo replaces a combo's strategy and target chain in-memory, in place.
// It preserves the combo's position in the list and its drain_ttl, and rejects
// unknown strategies or an empty target chain.
func SetCombo(c *Config, name, strategy string, targets []ComboTarget) error {
	if !validStrategies[strategy] {
		return fmt.Errorf("combo %q: unknown strategy %q", name, strategy)
	}
	if len(targets) == 0 {
		return fmt.Errorf("combo %q: at least one target is required", name)
	}
	for i := range c.Combos {
		if c.Combos[i].Name == name {
			c.Combos[i].Strategy = strategy
			c.Combos[i].Targets = targets
			return nil
		}
	}
	return fmt.Errorf("combo %q not found", name)
}

// UpdateCombo replaces a combo's strategy and target chain on disk.
func UpdateCombo(path, name, strategy string, targets []ComboTarget) error {
	return Mutate(path, func(c *Config) error {
		return SetCombo(c, name, strategy, targets)
	})
}
