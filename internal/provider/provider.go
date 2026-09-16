package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	Name      string
	BaseURL   string
	Models    []string
	Timeout   time.Duration
	Transport http.RoundTripper
}

type Credentials struct {
	APIKey       string
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
	ProjectID    string
	AccountID    string
	Email        string
}

func (c Credentials) Identity() string {
	if c.AccountID != "" {
		return c.AccountID
	}
	return c.Email
}

type CredStore interface {
	Get() Credentials
	Put(Credentials) error
}

type AccountStore interface {
	CredStore
	Accounts() []Credentials
	PutAccount(identity string, credentials Credentials) error
}

type Provider interface {
	Name() string
	ChatCompletion(ctx context.Context, req ChatRequest, w http.ResponseWriter) error
	Models(ctx context.Context) ([]Model, error)
	Test(ctx context.Context) TestResult
}

type Factory func(cfg Config, store CredStore) Provider

type HTTPStatusError struct {
	StatusCode int
	Message    string
}

func (e *HTTPStatusError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("upstream status %d", e.StatusCode)
}

func (e *HTTPStatusError) HTTPStatus() int {
	return e.StatusCode
}

func (e *HTTPStatusError) Drainable() bool {
	return !isClientError(e)
}

func (e *HTTPStatusError) DrainReason() string {
	return e.Error()
}

func NewHTTPStatusError(code int, msg string) *HTTPStatusError {
	return &HTTPStatusError{StatusCode: code, Message: msg}
}
