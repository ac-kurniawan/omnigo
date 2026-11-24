package provider

import "github.com/ac-kurniawan/omnigo/internal/effort"

// antigravityLevels is the fixed effort set Antigravity models accept. It is
// shared with the capability table so the picker and the capability report
// cannot drift apart.
var antigravityLevels = []string{"low", "medium", "high"}

// EffortVariants reports the reasoning-effort levels this provider accepts for
// base. It returns nil when the base is not effort-capable, so a caller can
// treat nil as "offer no levels".
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
// the family index once (see PickerModels) and call effortVariants.
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

// PickerEntry is one row in the combo picker: a model and the effort levels it
// offers. Levels is nil when the model takes no effort.
type PickerEntry struct {
	Model  string
	Levels []string
}

// PickerModels groups a provider's cached catalog into the picker's rows: one
// row per model, carrying the effort levels that model supports. A family the
// catalog lists only as level variants (gemini-3.7-flash-low/high) collapses
// into one row on its base, because Antigravity takes its effort in the model
// id; the plain base stays one row, so nothing that used to be selectable
// disappears. A model with no variants keeps its literal id and no levels.
//
// The result is deduplicated by model, in first-seen order.
func PickerModels(providerType string, models []string) []PickerEntry {
	families := effortFamilies(models)
	seen := make(map[string]bool, len(models))
	rows := make([]PickerEntry, 0, len(models))
	for _, model := range models {
		base := effort.Base(model)
		levels := effortVariants(providerType, base, families)
		entry := PickerEntry{Model: model}
		if len(levels) > 0 {
			// A level variant collapses onto its base row, which then carries
			// the level as a dropdown value instead of its own row.
			entry = PickerEntry{Model: base, Levels: levels}
		}
		if seen[entry.Model] {
			continue
		}
		seen[entry.Model] = true
		rows = append(rows, entry)
	}
	return rows
}
