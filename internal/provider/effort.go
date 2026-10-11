package provider

import "github.com/ac-kurniawan/omnigo/internal/effort"

// EffortVariants reports the reasoning-effort levels that can be appended to
// base to form selectable model ids on this provider. It returns nil when the
// base is not effort-capable, so a caller can treat nil as "offer no clones".
//
// The base is normalised first, so a suffixed id (gpt-5.5-high) resolves to the
// same variants as its base. Antigravity encodes effort in the upstream model
// id itself, so its base is only cloneable when the catalog already contains at
// least one level variant of it — that is the signal the upstream recognises
// the family, and it keeps a plain non-reasoning model from growing clones.
func EffortVariants(providerType, base string, catalog []string) []string {
	base = effort.Base(base)
	switch providerType {
	case "codex":
		if levels, _, ok := codexBuiltinProfile(base); ok {
			return dedupeLevels(levels)
		}
		return levelsOf(openAIReasoningCapabilities(base))
	case "openai":
		return levelsOf(openAIReasoningCapabilities(base))
	case "antigravity":
		if !antigravityFamilySupportsLevels(base, catalog) {
			return nil
		}
		return []string{"low", "medium", "high"}
	default:
		return nil
	}
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

// antigravityFamilySupportsLevels reports whether any catalog entry is a level
// variant of base (for example base "gemini-3.7-flash" and entry
// "gemini-3.7-flash-high"). A capability marker such as "-thinking" does not
// count, because it does not select an effort.
func antigravityFamilySupportsLevels(base string, catalog []string) bool {
	for _, id := range catalog {
		entryBase, suffix, found := effort.Split(id)
		if found && entryBase == base && effort.Level(suffix) {
			return true
		}
	}
	return false
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
	seen := make(map[string]bool, len(models))
	out := make([]EffortCatalogModel, 0, len(models))
	for _, model := range models {
		if seen[model] {
			continue
		}
		seen[model] = true
		out = append(out, EffortCatalogModel{ID: model, Label: model})
		levels := EffortVariants(providerType, model, models)
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
