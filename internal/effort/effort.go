// Package effort centralises the encoding of a reasoning-effort selection into
// a model id suffix, and the reverse. Antigravity encodes effort as a real
// upstream model id (`gemini-3.7-flash-high`), while Codex and OpenAI carry it
// in the request body (`reasoning_effort`). A combo target stores the suffixed
// id, so the gateway must be able to split it back into a base model plus an
// effort before dispatch.
package effort

import "strings"

// levels are the concrete reasoning efforts a model can be asked for, longest
// first so "-extra-low" is matched before the "-low" it ends with.
var levels = []string{"extra-low", "medium", "xhigh", "ultra", "high", "low", "max"}

// markers are capability suffixes that select a reasoning variant without
// naming an effort level. They split the same way but are not injectable
// efforts: the upstream model already decides its own reasoning budget.
var markers = []string{"thinking", "tiered"}

// Level reports whether s names a concrete reasoning effort.
func Level(s string) bool {
	for _, level := range levels {
		if s == level {
			return true
		}
	}
	return false
}

// Split separates a trailing effort suffix from a model id. It returns the base
// model, the suffix without its leading dash, and whether a suffix was found.
// An id that is entirely a suffix, or carries no recognised suffix, is returned
// unchanged with found=false so a caller never derives an empty base model.
func Split(model string) (base, suffix string, found bool) {
	for _, candidate := range append(append([]string{}, levels...), markers...) {
		marker := "-" + candidate
		if !strings.HasSuffix(model, marker) {
			continue
		}
		trimmed := strings.TrimSuffix(model, marker)
		if trimmed == "" {
			continue
		}
		return trimmed, candidate, true
	}
	return model, "", false
}

// Base returns the model id with any effort suffix removed, or the id itself
// when it carries none.
func Base(model string) string {
	base, _, _ := Split(model)
	return base
}

// Apply appends an effort suffix to a base model id.
func Apply(base, level string) string {
	if level == "" {
		return base
	}
	return base + "-" + level
}
