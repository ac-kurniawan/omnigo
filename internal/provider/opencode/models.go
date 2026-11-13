package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type protocol int

const (
	protocolChat protocol = iota
	protocolMessages
	protocolResponses
)

func classifyModel(model string) protocol {
	m := strings.ToLower(model)
	if strings.HasPrefix(m, "claude-") || strings.HasPrefix(m, "qwen") || strings.HasPrefix(m, "minimax-m3") {
		return protocolMessages
	}
	if strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "grok-") || strings.HasPrefix(m, "muse-spark-") {
		return protocolResponses
	}
	return protocolChat
}

// isGoAPI reports whether the provider points at the Go subscription API
// (https://opencode.ai/inference/go...) rather than the zen OAuth surface.
func isGoAPI(baseURL string) bool {
	return strings.Contains(baseURL, "/inference/go")
}

// goRoot returns the URL prefix through "/inference/go", so callers can
// append the per-model contract paths even when baseURL names a subpath.
func goRoot(baseURL string) string {
	if idx := strings.Index(baseURL, "/inference/go"); idx >= 0 {
		return baseURL[:idx+len("/inference/go")]
	}
	return strings.TrimRight(baseURL, "/")
}

func modelsURL(baseURL string) string {
	if isGoAPI(baseURL) {
		return goRoot(baseURL) + "/v1/models"
	}
	return strings.TrimRight(baseURL, "/") + "/models"
}

func endpointFor(baseURL string, proto protocol) string {
	if isGoAPI(baseURL) {
		root := goRoot(baseURL)
		switch proto {
		case protocolMessages:
			return root + "/anthropic/v1/messages"
		case protocolResponses:
			return root + "/openai/v1/responses"
		default:
			return root + "/openai/v1/chat/completions"
		}
	}
	switch proto {
	case protocolMessages:
		return strings.TrimRight(baseURL, "/") + "/messages"
	case protocolResponses:
		return strings.TrimRight(baseURL, "/") + "/responses"
	default:
		return strings.TrimRight(baseURL, "/") + "/chat/completions"
	}
}

type modelsWire struct {
	Data []modelWire `json:"data"`
}

type wireEffort struct {
	Efforts []string
	Default string
}

// UnmarshalJSON accepts the zen object shape {"efforts":[...],"default":...}
// and the Go models endpoint shape ["low","high",...].
func (e *wireEffort) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var efforts []string
		if err := json.Unmarshal(data, &efforts); err != nil {
			return err
		}
		e.Efforts = efforts
		return nil
	}
	var obj struct {
		Efforts []string `json:"efforts"`
		Default string   `json:"default"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	e.Efforts = obj.Efforts
	e.Default = obj.Default
	return nil
}

type modelWire struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Capabilities *struct {
		Reasoning *struct {
			Effort *wireEffort `json:"effort"`
		} `json:"reasoning"`
	} `json:"capabilities"`
	// API names the native contract per model on the Go endpoint, e.g.
	// {"responses": {...}}, {"messages": {...}}, {"chat-completions": {...}}.
	API map[string]json.RawMessage `json:"api"`
}

// protocolForWire prefers the endpoint's api contract; new or unknown models
// fall back to the prefix classifier.
func protocolForWire(m modelWire) protocol {
	for key := range m.API {
		switch strings.ToLower(key) {
		case "responses":
			return protocolResponses
		case "messages":
			return protocolMessages
		}
	}
	return classifyModel(m.ID)
}

func parseModels(r io.Reader) ([]provider.Model, error) {
	models, _, err := parseModelsWithProtocols(r)
	return models, err
}

func parseModelsWithProtocols(r io.Reader) ([]provider.Model, map[string]protocol, error) {
	body, err := provider.ReadBoundedBody(r)
	if err != nil {
		return nil, nil, err
	}
	var wire modelsWire
	if err := json.Unmarshal(body, &wire); err != nil || len(wire.Data) == 0 {
		// OpenCode or test mocks may send a flat array `[]modelWire` instead of `{"data": [...]}`
		var flat []modelWire
		if fErr := json.Unmarshal(body, &flat); fErr == nil {
			wire.Data = flat
		} else if err != nil {
			return nil, nil, err
		}
	}
	out := make([]provider.Model, 0, len(wire.Data))
	protos := make(map[string]protocol, len(wire.Data))
	for _, m := range wire.Data {
		if m.ID == "" {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		var caps *provider.ModelCapabilities
		if m.Capabilities != nil && m.Capabilities.Reasoning != nil {
			caps = &provider.ModelCapabilities{Reasoning: true}
			if m.Capabilities.Reasoning.Effort != nil {
				caps.ReasoningEfforts = append([]string(nil), m.Capabilities.Reasoning.Effort.Efforts...)
				caps.DefaultEffort = m.Capabilities.Reasoning.Effort.Default
				if caps.DefaultEffort == "" && len(caps.ReasoningEfforts) > 0 {
					caps.DefaultEffort = defaultEffort(caps.ReasoningEfforts)
				}
			}
		}
		out = append(out, provider.Model{
			ID:           m.ID,
			Name:         name,
			Capabilities: caps,
		})
		protos[m.ID] = protocolForWire(m)
	}
	return out, protos, nil
}

// defaultEffort prefers medium when offered, else the first effort level.
func defaultEffort(efforts []string) string {
	for _, e := range efforts {
		if e == "medium" {
			return "medium"
		}
	}
	return efforts[0]
}

func (p *Provider) Models(ctx context.Context) ([]provider.Model, error) {
	creds := p.store.Get()
	token := creds.APIKey
	if token == "" {
		token = creds.AccessToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL(p.baseURL), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", customUserAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, provider.NewHTTPStatusError(resp.StatusCode, "opencode: upstream status "+http.StatusText(resp.StatusCode))
	}
	models, protos, err := parseModelsWithProtocols(resp.Body)
	if err != nil {
		return nil, err
	}
	p.catalogMu.Lock()
	p.catalog = models
	p.protoRoutes = protos
	p.catalogMu.Unlock()
	return models, nil
}

func (p *Provider) Test(ctx context.Context) provider.TestResult {
	start := time.Now()
	_, err := p.Models(ctx)
	res := provider.TestResult{LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}
