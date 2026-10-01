package codex

import "github.com/ac-kurniawan/omnigo/internal/provider"

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
var builtinProfiles = map[string]modelProfile{
	"gpt-6-astra":   {Lite: true, Levels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, Default: "low"},
	"gpt-6.1-sol":   {Lite: true, Levels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, Default: "low"},
	"gpt-5.6-sol":   {Lite: true, Levels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, Default: "low"},
	"gpt-6-sol":     {Lite: true, Levels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, Default: "medium"},
	"gpt-5.6-terra": {Lite: true, Levels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, Default: "medium"},
	"gpt-6-luna":    {Lite: true, Levels: []string{"low", "medium", "high", "xhigh", "max"}, Default: "medium"},
	"gpt-5.6-luna":  {Lite: true, Levels: []string{"low", "medium", "high", "xhigh", "max"}, Default: "medium"},
	"gpt-5.5":       {Lite: false, Levels: []string{"low", "medium", "high", "xhigh"}, Default: "medium"},
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

func fallbackModels() []provider.Model {
	models := make([]provider.Model, 0, len(DefaultModels))
	for _, id := range DefaultModels {
		models = append(models, provider.Model{ID: id, Name: id})
	}
	return models
}
