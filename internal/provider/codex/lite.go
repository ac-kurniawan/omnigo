package codex

// applyModelProfile rewrites a request translated by ToResponsesRequest for the
// given model profile. It mutates out in place. Requests to non-Lite models
// whose effort is already valid come out unchanged, so the legacy transport
// stays byte-identical to what the translator produced.
func applyModelProfile(out map[string]any, profile modelProfile) {
	normaliseEffort(out, profile)
	if !profile.Lite {
		return
	}

	reasoning, ok := out["reasoning"].(map[string]any)
	if !ok {
		reasoning = map[string]any{"effort": profile.Default}
	} else if summary, exists := reasoning["summary"]; exists && summary == "auto" {
		// "auto" is what the translator injects for reasoning_effort; Codex
		// rejects it on Lite, while an explicit client summary is valid.
		delete(reasoning, "summary")
	}
	reasoning["context"] = "all_turns"
	out["reasoning"] = reasoning
	out["parallel_tool_calls"] = false

	var tools []any
	if raw, ok := out["tools"].([]any); ok {
		tools = raw
	}
	prefix := []any{map[string]any{"type": "additional_tools", "role": "developer", "tools": liteToolNamespaces(tools)}}
	if instructions, ok := out["instructions"].(string); ok {
		prefix = append(prefix, map[string]any{
			"type": "message", "role": "developer",
			"content": []any{map[string]any{"type": "input_text", "text": instructions}},
		})
	}
	input, _ := out["input"].([]any)
	out["input"] = append(prefix, input...)
	delete(out, "instructions")
	delete(out, "tools")

	stripImageDetail(out["input"].([]any))
}

// liteToolNamespaces wraps every function tool into the single "functions"
// namespace Lite expects; with no tools it sends an empty list.
func liteToolNamespaces(tools []any) []any {
	if len(tools) == 0 {
		return []any{}
	}
	return []any{map[string]any{
		"type": "namespace", "name": "functions", "description": "", "tools": tools,
	}}
}

// stripImageDetail drops the OpenAI-only detail hint from every input_image
// part, which Lite rejects.
func stripImageDetail(input []any) {
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || item["type"] != "message" {
			continue
		}
		content, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if ok && part["type"] == "input_image" {
				delete(part, "detail")
			}
		}
	}
}

// normaliseEffort maps a reasoning effort the model does not support onto one
// it does: none/minimal to the lowest level, ultra to max when listed and to
// the highest listed level otherwise, max to the highest listed level. A valid
// or absent effort is left untouched.
func normaliseEffort(out map[string]any, profile modelProfile) {
	if len(profile.Levels) == 0 {
		return
	}
	reasoning, ok := out["reasoning"].(map[string]any)
	if !ok {
		return
	}
	effort, ok := reasoning["effort"].(string)
	if !ok {
		return
	}
	for _, level := range profile.Levels {
		if effort == level {
			return
		}
	}
	highest := profile.Levels[len(profile.Levels)-1]
	switch {
	case effort == "none" || effort == "minimal":
		reasoning["effort"] = profile.Levels[0]
	case effort == "ultra" && hasLevel(profile.Levels, "max"):
		reasoning["effort"] = "max"
	default:
		reasoning["effort"] = highest
	}
}

func hasLevel(levels []string, level string) bool {
	for _, candidate := range levels {
		if candidate == level {
			return true
		}
	}
	return false
}
