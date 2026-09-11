package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type openAIProvider struct {
	name    string
	baseURL string
	store   CredStore
	client  *http.Client
}

func NewOpenAI(cfg Config, store CredStore) Provider {
	return &openAIProvider{name: cfg.Name, baseURL: strings.TrimRight(cfg.BaseURL, "/"), store: store, client: &http.Client{Timeout: 30 * time.Second}}
}

func (p *openAIProvider) Name() string { return p.name }

func (p *openAIProvider) Models(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	p.authorize(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: status %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(out.Data))
	for _, m := range out.Data {
		models = append(models, Model{ID: m.ID, Name: m.ID})
	}
	return models, nil
}

func (p *openAIProvider) Test(ctx context.Context) TestResult {
	start := time.Now()
	_, err := p.Models(ctx)
	res := TestResult{LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}

func (p *openAIProvider) ChatCompletion(ctx context.Context, req ChatRequest, w http.ResponseWriter) error {
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", strings.NewReader(string(req.Raw)))
	if err != nil {
		return err
	}
	up.Header.Set("Content-Type", "application/json")
	p.authorize(up)
	resp, err := p.client.Do(up)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	return err
}

func (p *openAIProvider) authorize(req *http.Request) {
	if c := p.store.Get(); c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
}

func init() {
	Register("openai", func(cfg Config, store CredStore) Provider { return NewOpenAI(cfg, store) })
}
