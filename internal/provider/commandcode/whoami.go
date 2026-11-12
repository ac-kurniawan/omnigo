package commandcode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type WhoAmI struct {
	UserID   string
	UserName string
	Email    string
	OrgID    string
	PlanID   string
	PlanName string
}

func apiBaseFor(baseURL string) string {
	return deriveAPIBase(baseURL)
}

func ValidateKey(ctx context.Context, client *http.Client, apiBase, key string) (WhoAmI, error) {
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/alpha/whoami?limits=1", nil)
	if err != nil {
		return WhoAmI{}, err
	}
	for k, v := range buildCommandCodeHeaders(key, "", "application/json") {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	if err != nil {
		return WhoAmI{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return WhoAmI{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return WhoAmI{}, fmt.Errorf("commandcode: whoami status %d", resp.StatusCode)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return WhoAmI{}, fmt.Errorf("commandcode: decode whoami: %w", err)
	}
	out := WhoAmI{}
	if v, _ := raw["userId"].(string); v != "" {
		out.UserID = v
	} else if v, _ := raw["user_id"].(string); v != "" {
		out.UserID = v
	}
	if v, _ := raw["userName"].(string); v != "" {
		out.UserName = v
	} else if v, _ := raw["username"].(string); v != "" {
		out.UserName = v
	}
	if v, _ := raw["email"].(string); v != "" {
		out.Email = v
	}
	if org, ok := raw["org"].(map[string]any); ok {
		if v, _ := org["id"].(string); v != "" {
			out.OrgID = v
		}
		if v, _ := org["planId"].(string); v != "" {
			out.PlanID = v
		}
		if v, _ := org["plan"].(string); v != "" {
			out.PlanName = v
		}
	}
	if out.UserName == "" {
		out.UserName = out.Email
	}
	return out, nil
}
