package provider

import (
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/provider/codexprofile"
)

func CapabilitiesFor(providerType, modelID string) *ModelCapabilities {
	switch providerType {
	case "codex":
		if levels, defaultEffort, ok := codexBuiltinProfile(modelID); ok {
			return capabilitiesFromLevels(levels, defaultEffort)
		}
		return openAIReasoningCapabilities(modelID)
	case "antigravity":
		return antigravityCapabilities(modelID)
	case "openai":
		return openAIReasoningCapabilities(modelID)
	default:
		return nil
	}
}

func codexBuiltinProfile(model string) ([]string, string, bool) {
	profile, ok := codexprofile.Known(model)
	if !ok {
		return nil, "", false
	}
	return profile.Levels, profile.Default, true
}

func capabilitiesFromLevels(levels []string, defaultEffort string) *ModelCapabilities {
	copied := make([]string, 0, len(levels))
	for _, level := range levels {
		if level != "" {
			copied = append(copied, level)
		}
	}
	if len(copied) == 0 {
		return nil
	}
	return &ModelCapabilities{Reasoning: true, ReasoningEfforts: copied, DefaultEffort: defaultEffort}
}

func openAIReasoningCapabilities(id string) *ModelCapabilities {
	id = strings.ToLower(id)
	if !openAIReasoningID(id) {
		return nil
	}
	return capabilitiesFromLevels([]string{"low", "medium", "high"}, "medium")
}

func openAIReasoningID(id string) bool {
	for _, prefix := range []string{"o1", "o3", "o4"} {
		if id == prefix || strings.HasPrefix(id, prefix+"-") {
			return true
		}
	}
	if strings.Contains(id, "grok-4") || strings.Contains(id, "deepseek-r1") || strings.Contains(id, "deepseek-v4") {
		return true
	}
	return strings.Contains(id, "-thinking") || strings.HasSuffix(id, "thinking")
}

func antigravityCapabilities(id string) *ModelCapabilities {
	id = strings.ToLower(id)
	if id == "claude-sonnet-4-6" || id == "gemini-3.1-flash-lite" {
		return nil
	}
	if strings.Contains(id, "-thinking") || antigravityEffortSuffix(id) {
		return capabilitiesFromLevels(antigravityLevels, "medium")
	}
	return nil
}

func antigravityEffortSuffix(id string) bool {
	for _, suffix := range []string{"-high", "-medium", "-low", "-extra-low", "-tiered"} {
		if strings.HasSuffix(id, suffix) || strings.Contains(id, suffix+"-") {
			return true
		}
	}
	return false
}
