package codex

import (
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/provider/codexprofile"
)

// modelProfile records how a model must be transported: whether it requires
// the Responses Lite wire format and which reasoning levels it accepts.
type modelProfile struct {
	Lite    bool
	Levels  []string
	Default string
}

// builtinProfiles snapshots the catalog as of today. profile() falls back to
// it until the first successful Models() fetch installs the live catalog, so
// a fresh process routes correctly without waiting on the network.
var builtinProfiles = builtinProfilesFromSource()

func builtinProfilesFromSource() map[string]modelProfile {
	profiles := map[string]modelProfile{}
	for _, id := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-5.6-sol", "gpt-6-sol", "gpt-5.6-terra", "gpt-6-luna", "gpt-5.6-luna", "gpt-5.5"} {
		src, ok := codexprofile.Known(id)
		if !ok {
			panic("codex: missing builtin profile " + id)
		}
		profiles[id] = modelProfile{Lite: id != "gpt-5.5", Levels: append([]string(nil), src.Levels...), Default: src.Default}
	}
	return profiles
}

// ponytail: models unknown to both tables use the legacy transport until the
// next Refresh models; the upgrade path is a lazy catalog fetch on a miss.
func (p *Provider) profile(model string) (modelProfile, bool) {
	p.profileMu.RLock()
	defer p.profileMu.RUnlock()
	if profile, ok := p.profiles[model]; ok {
		return profile, true
	}
	profile, ok := builtinProfiles[model]
	return profile, ok
}

func KnownProfile(model string) (modelProfile, bool) {
	profile, ok := builtinProfiles[model]
	return profile, ok
}

func capabilitiesFromLevels(levels []string, defaultEffort string) *provider.ModelCapabilities {
	copied := make([]string, 0, len(levels))
	for _, level := range levels {
		if level != "" {
			copied = append(copied, level)
		}
	}
	if len(copied) == 0 {
		return nil
	}
	return &provider.ModelCapabilities{Reasoning: true, ReasoningEfforts: copied, DefaultEffort: defaultEffort}
}

func fallbackModels() []provider.Model {
	models := make([]provider.Model, 0, len(DefaultModels))
	for _, id := range DefaultModels {
		model := provider.Model{ID: id, Name: id}
		if profile, ok := KnownProfile(id); ok {
			model.Capabilities = capabilitiesFromLevels(profile.Levels, profile.Default)
		}
		models = append(models, model)
	}
	return models
}
