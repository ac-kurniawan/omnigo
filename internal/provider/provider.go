package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type ChatRequest struct {
	Model    string
	Stream   bool
	Messages []Message
	Raw      []byte
}

// Body returns the request JSON payload intended for upstream forwarding,
// ensuring the "model" field reflects req.Model (the resolved bare model id)
// and normalizes "developer" role messages to "system" for broad provider compatibility.
func (r ChatRequest) Body() ([]byte, error) {
	if len(r.Raw) > 0 {
		var rawMap map[string]any
		dec := json.NewDecoder(bytes.NewReader(r.Raw))
		dec.UseNumber()
		if err := dec.Decode(&rawMap); err == nil {
			rawMap["model"] = r.Model
			if rawMsgs, ok := rawMap["messages"].([]any); ok {
				for _, item := range rawMsgs {
					if msgMap, ok := item.(map[string]any); ok {
						if role, ok := msgMap["role"].(string); ok && role == "developer" {
							msgMap["role"] = "system"
						}
					}
				}
			}
			return json.Marshal(rawMap)
		}
	}
	msgs := make([]Message, len(r.Messages))
	copy(msgs, r.Messages)
	for i := range msgs {
		if msgs[i].Role == "developer" {
			msgs[i].Role = "system"
		}
	}
	return json.Marshal(map[string]any{
		"model":    r.Model,
		"stream":   r.Stream,
		"messages": msgs,
	})
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
	Timeout time.Duration
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
