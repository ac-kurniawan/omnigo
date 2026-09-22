package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

// unaryClient bounds the OAuth and metadata calls so a hung Google endpoint
// cannot leak goroutines indefinitely. Streaming completions use their own
// client with the configured provider timeout.
var unaryClient = &http.Client{Timeout: 15 * time.Second}

// defaultBaseURL is the Cloud Code host the official Antigravity CLI uses.
// The legacy cloudcode-pa.googleapis.com host returns a detail-free
// 429 RESOURCE_EXHAUSTED for consumer accounts even when quota remains;
// daily-cloudcode-pa.googleapis.com serves the same API and completes.
const defaultBaseURL = "https://daily-cloudcode-pa.googleapis.com"

// baseURL is a var so tests can override it with an httptest server.
var baseURL = defaultBaseURL

const (
	antigravityUserAgent = "antigravity/ide/0.0.0 darwin/arm64 google-api-nodejs-client/10.3.0"
	antigravityGoogAPI   = "gl-node/22.21.1"
)

// DefaultAntigravityModels is the curated fallback list of Antigravity models.
// If the upstream live catalog is temporarily empty or restricted, this ensures
// users still have the full available model list.
var DefaultAntigravityModels = []string{
	"gemini-3.7-flash-medium",
	"gemini-3.7-flash-high",
	"gemini-3.7-flash-low",
	"gemini-3.1-pro-low",
	"gemini-3.1-flash-lite",
	"claude-sonnet-4-6",
	"claude-opus-4-6-thinking",
	"gpt-oss-120b-medium",
}

func post(ctx context.Context, path, accessToken string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", antigravityUserAgent)
	req.Header.Set("X-Goog-Api-Client", antigravityGoogAPI)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	return unaryClient.Do(req)
}

func extractProjectID(bodyBytes []byte) string {
	var raw map[string]any
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		return ""
	}
	p, ok := raw["cloudaicompanionProject"]
	if !ok || p == nil {
		return ""
	}
	if s, ok := p.(string); ok && s != "" {
		return s
	}
	if m, ok := p.(map[string]any); ok {
		if id, ok := m["id"].(string); ok && id != "" {
			return id
		}
	}
	return ""
}

func DiscoverProject(ctx context.Context, accessToken string) (string, error) {
	meta := map[string]any{"ideType": 9, "platform": 3, "pluginType": 2}
	resp, err := post(ctx, "/v1internal:loadCodeAssist", accessToken, map[string]any{
		"metadata": meta,
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("loadCodeAssist: status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	pid := extractProjectID(b)
	if pid != "" {
		return pid, nil
	}

	// If project is not yet provisioned, attempt onboardUser and re-query loadCodeAssist
	onboardResp, err := post(ctx, "/v1internal:onboardUser", accessToken, map[string]any{
		"tier_id":  "legacy-tier",
		"metadata": meta,
	})
	if err == nil {
		onboardResp.Body.Close()
		retryResp, err := post(ctx, "/v1internal:loadCodeAssist", accessToken, map[string]any{
			"metadata": meta,
		})
		if err == nil {
			defer retryResp.Body.Close()
			if rb, err := io.ReadAll(retryResp.Body); err == nil {
				if rpid := extractProjectID(rb); rpid != "" {
					return rpid, nil
				}
			}
		}
	}

	return pid, nil
}

// parseModelsResponse extracts model IDs from the /v1internal:fetchAvailableModels response.
// Google returns an object map {"models": {"gemini-3.7-flash": {...}}}, but array shapes
// are also supported.
func parseModelsResponse(bodyBytes []byte) []string {
	var raw map[string]any
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		return nil
	}
	modelsVal, ok := raw["models"]
	if !ok || modelsVal == nil {
		return nil
	}

	var ids []string
	// Case 1: models is a map/object: {"model-id": {...}}
	if m, ok := modelsVal.(map[string]any); ok {
		for id := range m {
			if id != "" {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		return ids
	}

	// Case 2: models is an array: [{"name": "id"} or "id"]
	if arr, ok := modelsVal.([]any); ok {
		for _, item := range arr {
			if s, ok := item.(string); ok && s != "" {
				ids = append(ids, s)
			} else if obj, ok := item.(map[string]any); ok {
				if name, ok := obj["name"].(string); ok && name != "" {
					ids = append(ids, name)
				} else if id, ok := obj["id"].(string); ok && id != "" {
					ids = append(ids, id)
				}
			}
		}
		return ids
	}

	return nil
}

func fetchModels(ctx context.Context, accessToken, projectID string) ([]string, error) {
	reqBody := map[string]any{}
	if projectID != "" {
		reqBody["project"] = projectID
	}
	resp, err := post(ctx, "/v1internal:fetchAvailableModels", accessToken, reqBody)
	if err != nil {
		return DefaultAntigravityModels, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Fallback to default catalog on error status (e.g. 403 or 404)
		return DefaultAntigravityModels, nil
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return DefaultAntigravityModels, nil
	}
	ids := parseModelsResponse(b)
	if len(ids) == 0 {
		return DefaultAntigravityModels, nil
	}
	return ids, nil
}
