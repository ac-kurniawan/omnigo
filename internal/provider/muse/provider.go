package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

const DefaultBaseURL = "https://api.meta.ai/v1"

type museProvider struct {
	name          string
	baseURL       string
	store         provider.CredStore
	client        *http.Client
	stream        *http.Client
	idle          time.Duration
	streamTimeout time.Duration
}

func init() {
	provider.Register("muse", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return New(cfg, store)
	})
}

func New(cfg provider.Config, store provider.CredStore) provider.Provider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	client := &http.Client{Timeout: timeout, Transport: cfg.Transport}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return &museProvider{
		name:          cfg.Name,
		baseURL:       base,
		store:         store,
		client:        client,
		stream:        provider.StreamClient(client),
		idle:          timeout,
		streamTimeout: cfg.StreamTimeout,
	}
}

func (p *museProvider) Name() string { return p.name }

func (p *museProvider) apiKey() (string, bool) {
	if p.store == nil {
		return "", false
	}
	if as, ok := p.store.(provider.AccountStore); ok {
		accounts := as.Accounts()
		for _, a := range accounts {
			if a.APIKey != "" {
				return a.APIKey, true
			}
			if a.AccessToken != "" {
				return a.AccessToken, true
			}
		}
	}
	creds := p.store.Get()
	if creds.APIKey != "" {
		return creds.APIKey, true
	}
	if creds.AccessToken != "" {
		return creds.AccessToken, true
	}
	return "", false
}

func (p *museProvider) ChatCompletion(ctx context.Context, req provider.ChatRequest, w http.ResponseWriter) error {
	apiKey, hasKey := p.apiKey()
	if !hasKey {
		return errors.New("no API key configured for muse provider")
	}

	body, err := req.Body()
	if err != nil {
		return fmt.Errorf("encode request body: %w", err)
	}

	endpoint := p.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("x-api-version", APIVersion)
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}

	client := p.client
	if req.Stream {
		client = p.stream
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("forward request to muse: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		provider.WarnUpstreamError(p.name, req.Model, "", resp.StatusCode, provider.UpstreamSnippet(resp.Body))
		return provider.NewHTTPStatusError(resp.StatusCode, fmt.Sprintf("upstream status %d", resp.StatusCode))
	}

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if req.Stream {
		flusher, _ := w.(http.Flusher)
		_, err = io.Copy(w, resp.Body)
		if flusher != nil {
			flusher.Flush()
		}
		return err
	}

	_, err = io.Copy(w, resp.Body)
	return err
}

func (p *museProvider) Models(ctx context.Context) ([]provider.Model, error) {
	apiKey, _ := p.apiKey()
	endpoint := p.baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-version", APIVersion)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models query failed with status %d", resp.StatusCode)
	}

	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}

	res := make([]provider.Model, 0, len(data.Data))
	for _, m := range data.Data {
		res = append(res, provider.Model{
			ID:   m.ID,
			Name: m.ID,
		})
	}
	return res, nil
}

func (p *museProvider) Test(ctx context.Context) provider.TestResult {
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
