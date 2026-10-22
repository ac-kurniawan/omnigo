package config

import (
	"strings"
	"testing"
	"time"
)

// baseSmartConfig is the smallest valid config with one smart combo, so each
// test can mutate exactly one field and attribute a failure to it.
func baseSmartConfig() *Config {
	return &Config{
		Providers: []Provider{
			{Name: "agy", Type: "antigravity", Models: []string{"gemini-3.8-flash", "gemini-3.1-pro"}},
			{Name: "codex", Type: "codex", Models: []string{"gpt-6-sol"}},
		},
		Combos: []Combo{
			{
				Name:     "combo/flash",
				Strategy: "priority",
				Targets:  []ComboTarget{{Provider: "agy", Model: "gemini-3.8-flash"}},
			},
			{
				Name:     "combo/auto",
				Strategy: "priority",
				Targets:  []ComboTarget{{Provider: "codex", Model: "gpt-6-sol"}},
			},
			{
				Name:         "combo/smart",
				Strategy:     "smart",
				DefaultRoute: "flash",
				Jev:          SmartJev{Model: "jev-latest", Timeout: "4s", MinConfidence: "0.5"},
				Routes: []SmartRoute{
					{Name: "flash", Criteria: "Simple, well-scoped request.", Destination: "combo/flash"},
					{Name: "pro", Criteria: "Complex request needing deep reasoning.", Destination: "combo/auto"},
				},
			},
		},
	}
}

func TestValidateSmartComboAccepts(t *testing.T) {
	if err := baseSmartConfig().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateSmartComboRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name:   "no routes",
			mutate: func(c *Config) { c.Combos[2].Routes = nil },
			want:   "at least one route",
		},
		{
			name:   "route missing name",
			mutate: func(c *Config) { c.Combos[2].Routes[0].Name = "" },
			want:   "route name is required",
		},
		{
			name:   "route missing criteria",
			mutate: func(c *Config) { c.Combos[2].Routes[0].Criteria = "   " },
			want:   "criteria is required",
		},
		{
			name:   "route missing destination",
			mutate: func(c *Config) { c.Combos[2].Routes[0].Destination = "" },
			want:   "destination is required",
		},
		{
			name: "duplicate route names",
			mutate: func(c *Config) {
				c.Combos[2].Routes[1].Name = c.Combos[2].Routes[0].Name
			},
			want: "duplicate route",
		},
		{
			name:   "default route not a route",
			mutate: func(c *Config) { c.Combos[2].DefaultRoute = "nope" },
			want:   "default_route",
		},
		{
			name:   "smart combo must not declare targets",
			mutate: func(c *Config) { c.Combos[2].Targets = []ComboTarget{{Provider: "agy", Model: "gemini-3.8-flash"}} },
			want:   "must not declare targets",
		},
		{
			name: "plain combo must not declare routes",
			mutate: func(c *Config) {
				c.Combos[0].Routes = []SmartRoute{{Name: "x", Criteria: "y", Destination: "agy/gemini-3.8-flash"}}
			},
			want: "must not declare routes",
		},
		{
			name:   "route to itself",
			mutate: func(c *Config) { c.Combos[2].Routes[0].Destination = "combo/smart" },
			want:   "itself",
		},
		{
			name:   "unknown destination combo",
			mutate: func(c *Config) { c.Combos[2].Routes[0].Destination = "combo/missing" },
			want:   "unknown destination",
		},
		{
			name:   "route to a smart combo",
			mutate: func(c *Config) { c.Combos[2].Routes[1].Destination = "combo/smart" },
			want:   "itself",
		},
		{
			name:   "unknown destination provider",
			mutate: func(c *Config) { c.Combos[2].Routes[0].Destination = "nope/model" },
			want:   "unknown destination",
		},
		{
			name:   "bad jev timeout",
			mutate: func(c *Config) { c.Combos[2].Jev.Timeout = "not-a-duration" },
			want:   "invalid jev timeout",
		},
		{
			name:   "bad min_confidence",
			mutate: func(c *Config) { c.Combos[2].Jev.MinConfidence = "1.5" },
			want:   "invalid jev min_confidence",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseSmartConfig()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// A smart route may target a provider/model directly, with no combo in between.
func TestValidateSmartRouteDirectProviderDestination(t *testing.T) {
	cfg := baseSmartConfig()
	cfg.Combos[2].Routes[0].Destination = "agy/gemini-3.8-flash"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// A smart route to a smart combo would recurse; one level only.
func TestValidateSmartRouteCannotTargetAnotherSmartCombo(t *testing.T) {
	cfg := baseSmartConfig()
	cfg.Combos = append(cfg.Combos, Combo{
		Name:         "combo/smart2",
		Strategy:     "smart",
		DefaultRoute: "flash",
		Routes: []SmartRoute{
			{Name: "flash", Criteria: "c", Destination: "combo/flash"},
		},
	})
	cfg.Combos[2].Routes[0].Destination = "combo/smart2"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want a recursion rejection")
	}
	if !strings.Contains(err.Error(), "smart") {
		t.Fatalf("Validate() = %v, want a message naming the smart target", err)
	}
}

func TestSmartJevParsedDefaults(t *testing.T) {
	var zero SmartJev
	if got := zero.ParsedTimeout(); got != 4*time.Second {
		t.Errorf("ParsedTimeout() = %v, want 4s", got)
	}
	if got := zero.ParsedMinConfidence(); got != 0.5 {
		t.Errorf("ParsedMinConfidence() = %v, want 0.5", got)
	}
	if got := zero.ModelOrDefault(); got != "jev-latest" {
		t.Errorf("ModelOrDefault() = %q, want jev-latest", got)
	}
	if got := zero.QuestionOrDefault(); got == "" {
		t.Error("QuestionOrDefault() is empty")
	}

	j := SmartJev{Model: "jev-1.13.0", Timeout: "9s", MinConfidence: "0.8", Question: "Pick."}
	if got := j.ParsedTimeout(); got != 9*time.Second {
		t.Errorf("ParsedTimeout() = %v, want 9s", got)
	}
	if got := j.ParsedMinConfidence(); got != 0.8 {
		t.Errorf("ParsedMinConfidence() = %v, want 0.8", got)
	}
	if got := j.ModelOrDefault(); got != "jev-1.13.0" {
		t.Errorf("ModelOrDefault() = %q", got)
	}
	if got := j.QuestionOrDefault(); got != "Pick." {
		t.Errorf("QuestionOrDefault() = %q", got)
	}
}

// DestinationIsCombo distinguishes a combo name from a provider/model pair by
// asking the config which combos exist, because combo names may contain a slash
// (combo/flash) and so may provider/model ids (agy/gemini-3.8-flash).
func TestComboDestinationResolution(t *testing.T) {
	cfg := baseSmartConfig()
	smart := cfg.Combos[2]

	route := smart.Routes[0]
	if !route.DestinationIsCombo(cfg.Combos) {
		t.Errorf("combo/flash should resolve to a combo")
	}

	direct := SmartRoute{Name: "x", Criteria: "y", Destination: "agy/gemini-3.8-flash"}
	if direct.DestinationIsCombo(cfg.Combos) {
		t.Errorf("agy/gemini-3.8-flash should not resolve to a combo")
	}
}

func TestFindComboByName(t *testing.T) {
	cfg := baseSmartConfig()
	cb, ok := FindCombo(cfg, "combo/flash")
	if !ok || cb.Name != "combo/flash" {
		t.Fatalf("FindCombo = %v, %v", cb.Name, ok)
	}
	if _, ok := FindCombo(cfg, "nope"); ok {
		t.Error("FindCombo found a combo that does not exist")
	}
}
