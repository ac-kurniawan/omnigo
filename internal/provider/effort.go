package provider

import "github.com/ac-kurniawan/omnigo/internal/effort"

// antigravityLevels is the fixed effort set Antigravity models accept. It is
// shared with the capability table so the picker and the capability report
// cannot drift apart.
var antigravityLevels = []string{"low", "medium", "high"}

// EffortVariants reports the reasoning-effort levels that can be appended to
// base to form selectable model ids on this provider. It returns nil when the
// base is not effort-capable, so a caller can treat nil as "offer no clones".
//
// The base is normalised first, so a suffixed id (gpt-5.5-high) resolves to the
// same variants as its base. Antigravity encodes effort in the upstream model
// id itself, so its base is only cloneable when the catalog already contains at
// least one level variant of it — that is the signal the upstream recognises
// the family, and it keeps a plain non-reasoning model from growing clones.
// Every other provider derives its levels from the same capability table the
// API reports, so the picker and /v1/models stay in sync.
//
// A caller that resolves variants for many models of one catalog should build
// the family index once (see ModelsWithEffort) and call effortVariants.
func EffortVariants(providerType, base string, catalog []string) []string {
	return effortVariants(providerType, effort.Base(base), effortFamilies(catalog))
}

func effortVariants(providerType, base string, families map[string]bool) []string {
	if providerType == "antigravity" {
		if !families[base] {
			return nil
		}
		return append([]string(nil), antigravityLevels...)
	}
	return levelsOf(CapabilitiesFor(providerType, base))
}

// effortFamilies indexes which model families have at least one level variant in
// catalog (base "gemini-3.7-flash" is present when "gemini-3.7-flash-high" is).
// It is built once per catalog so resolving variants for every model is linear
// rather than rescanning the catalog per model. A capability marker such as
// "-thinking" is not a level and does not mark a family cloneable.
func effortFamilies(catalog []string) map[string]bool {
	families := make(map[string]bool, len(catalog))
	for _, id := range catalog {
		base, suffix, found := effort.Split(id)
		if found && effort.Level(suffix) {
			families[base] = true
		}
	}
	return families
}

func levelsOf(caps *ModelCapabilities) []string {
	if caps == nil {
		return nil
	}
	return dedupeLevels(caps.ReasoningEfforts)
}

// dedupeLevels keeps only concrete effort levels, in first-seen order, so a
// profile that repeats a level or carries a non-effort token never yields a
// duplicate or unselectable variant.
func dedupeLevels(levels []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, level := range levels {
		if level == "" || seen[level] || !effort.Level(level) {
			continue
		}
		seen[level] = true
		out = append(out, level)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EffortCatalogModel is one selectable picker entry: the id written into the
// combo target, the label shown, and whether the id is a synthesised effort
// variant of a listed model rather than a catalog entry itself.
type EffortCatalogModel struct {
	ID     string
	Label  string
	Effort bool
}

// ModelsWithEffort expands a provider's cached catalog into the model ids the
// combo picker offers: each listed model, followed by a clone for every effort
// level its family supports. The effort clones are additive — the picker keeps
// the plain entry too, so nothing that used to be selectable disappears.
//
// The result is deduplicated by id: a family whose catalog already lists every
// level (gemini-3.7-flash-low/medium/high) must not show a duplicate clone for
// each variant, and a real catalog entry always wins over a synthesised one.
func ModelsWithEffort(providerType string, models []string) []EffortCatalogModel {
	listed := make(map[string]bool, len(models))
	for _, model := range models {
		listed[model] = true
	}
	// Index the families once so resolving variants is linear in the catalog,
	// not a rescan per model.
	families := effortFamilies(models)
	seen := make(map[string]bool, len(models))
	out := make([]EffortCatalogModel, 0, len(models))
	for _, model := range models {
		if seen[model] {
			continue
		}
		seen[model] = true
		out = append(out, EffortCatalogModel{ID: model, Label: model})
		levels := effortVariants(providerType, effort.Base(model), families)
		if len(levels) == 0 {
			continue
		}
		base := effort.Base(model)
		for _, level := range levels {
			id := effort.Apply(base, level)
			if listed[id] || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, EffortCatalogModel{ID: id, Label: id, Effort: true})
		}
	}
	return out
}
