package provider

import (
	"context"
	"net/http"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model    string
	Stream   bool
	Messages []Message
	Raw      []byte
}

type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type TestResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

type Config struct {
	Name    string
	BaseURL string
	Models  []string
}

type Credentials struct {
	APIKey       string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	ProjectID    string
}

// CredStore reads and persists one provider's credentials.
type CredStore interface {
	Get() Credentials
	Put(Credentials) error
}

type Provider interface {
	Name() string
	ChatCompletion(ctx context.Context, req ChatRequest, w http.ResponseWriter) error
	Models(ctx context.Context) ([]Model, error)
	Test(ctx context.Context) TestResult
}

type Factory func(cfg Config, store CredStore) Provider
