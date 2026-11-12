package commandcode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// DefaultGoModels is the static fallback catalog of Go-plan models, used when
// the dynamic Provider API catalog cannot be fetched. It mirrors the 9router
// registry ids.
var DefaultGoModels = []string{
	"commandcode/gpt-5.6",
	"commandcode/gpt-5.5",
	"commandcode/gpt-5.4",
	"commandcode/gpt-5.3",
	"commandcode/claude-opus-4-6",
	"commandcode/claude-sonnet-4-6",
	"commandcode/claude-haiku-4-5",
	"commandcode/gemini-3.5-pro",
	"commandcode/gemini-3.5-flash",
	"commandcode/grok-4.1",
	"commandcode/grok-4",
	"commandcode/llama-4-maverick",
	"commandcode/llama-4-scout",
	"commandcode/deepseek-v4",
	"commandcode/qwen-3-max",
	"commandcode/kimi-k2.5",
	"commandcode/minimax-m2.1",
	"commandcode/glm-4.6",
	"commandcode/solar-pro-2",
	"commandcode/gpt-oss-120b",
	"commandcode/qwen-3-235b",
	"commandcode/devstral-2",
	"commandcode/mistral-large-3",
	"commandcode/command-r-plus",
}

const modelsCacheTTL = 10 * time.Minute

type modelsCache struct {
	at     time.Time
	models []provider.Model
}

func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	p.mu.RLock()
	if len(p.modelCache) > 0 && time.Since(p.modelCacheAt) < modelsCacheTTL {
		out := append([]provider.Model(nil), p.modelCache...)
		p.mu.RUnlock()
		return out, nil
	}
	p.mu.RUnlock()
	models := p.fetchModels(ctx)
	p.mu.Lock()
	p.modelCache = append([]provider.Model(nil), models...)
	p.modelCacheAt = time.Now()
	p.mu.Unlock()
	return models, nil
}

func (p *Provider) fetchModels(ctx context.Context) []provider.Model {
	if p.modelsURL == "" {
		return p.fallbackModels()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.modelsURL, nil)
	if err != nil {
		return p.fallbackModels()
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return p.fallbackModels()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p.fallbackModels()
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return p.fallbackModels()
	}
	if models := parseModelsCatalog(raw); len(models) > 0 {
		return models
	}
	return p.fallbackModels()
}

func parseModelsCatalog(raw []byte) []provider.Model {
	var wrapped struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil
	}
	if len(wrapped.Data) > 0 {
		models := make([]provider.Model, 0, len(wrapped.Data))
		seen := map[string]bool{}
		for _, m := range wrapped.Data {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			name := m.Name
			if name == "" {
				name = m.ID
			}
			models = append(models, provider.Model{ID: m.ID, Name: name})
		}
		return models
	}
	var bare []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &bare); err != nil {
		return nil
	}
	models := make([]provider.Model, 0, len(bare))
	seen := map[string]bool{}
	for _, m := range bare {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		name := m.Name
		if name == "" {
			name = m.ID
		}
		models = append(models, provider.Model{ID: m.ID, Name: name})
	}
	return models
}

func (p *Provider) fallbackModels() []provider.Model {
	if len(p.configModels) > 0 {
		out := make([]provider.Model, 0, len(p.configModels))
		for _, id := range p.configModels {
			out = append(out, provider.Model{ID: id, Name: id})
		}
		return out
	}
	out := make([]provider.Model, 0, len(DefaultGoModels))
	for _, id := range DefaultGoModels {
		out = append(out, provider.Model{ID: id, Name: id})
	}
	return out
}
