package commandcode

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

const (
	DefaultGenerateURL = "https://api.commandcode.ai/alpha/generate"
	DefaultBaseURL     = "https://api.commandcode.ai"
	DefaultModelsURL   = "https://api.commandcode.ai/provider/v1/models"
)

type Provider struct {
	name          string
	generateURL   string
	apiBase       string
	modelsURL     string
	client        *http.Client
	stream        *http.Client
	idle          time.Duration
	streamTimeout time.Duration
	store         provider.CredStore
	pool          provider.AccountPool
	mu            sync.RWMutex
	modelCache    []provider.Model
	modelCacheAt  time.Time
	configModels  []string
}

func New(cfg provider.Config, store provider.CredStore) provider.Provider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	client := &http.Client{Timeout: timeout, Transport: cfg.Transport}
	generateURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if generateURL == "" {
		generateURL = DefaultGenerateURL
	}
	return &Provider{
		name:          cfg.Name,
		generateURL:   generateURL,
		apiBase:       deriveAPIBase(cfg.BaseURL),
		modelsURL:     DefaultModelsURL,
		client:        client,
		stream:        provider.StreamClient(client),
		idle:          timeout,
		streamTimeout: cfg.StreamTimeout,
		store:         store,
		configModels:  append([]string(nil), cfg.Models...),
	}
}

func (p *Provider) Name() string { return p.name }

// Test probes each candidate account with a real authenticated whoami
// roundtrip — the same authenticated surface ChatCompletion depends on. A
// stored key that the upstream rejects fails the probe. Failures do not
// MarkFailed the pool: a manual probe must not cool an account that live
// traffic may still serve.
func (p *Provider) Test(ctx context.Context) provider.TestResult {
	start := time.Now()
	accounts := p.pool.Available(p.store)
	var lastErr error
	for _, account := range accounts {
		if apiKey(account) == "" {
			lastErr = fmt.Errorf("commandcode: not authenticated")
			continue
		}
		_, lastErr = p.fetchWhoami(ctx, apiKey(account))
		if lastErr == nil {
			return provider.TestResult{OK: true, LatencyMS: time.Since(start).Milliseconds()}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("commandcode: not authenticated")
	}
	return provider.TestResult{LatencyMS: time.Since(start).Milliseconds(), Error: lastErr.Error()}
}

func apiKey(c provider.Credentials) string {
	if c.APIKey != "" {
		return c.APIKey
	}
	return c.AccessToken
}

func deriveAPIBase(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return DefaultBaseURL
	}
	if strings.HasSuffix(base, "/alpha/generate") {
		return strings.TrimSuffix(base, "/alpha/generate")
	}
	return base
}

func init() {
	provider.Register("commandcode", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return New(cfg, store)
	})
}
