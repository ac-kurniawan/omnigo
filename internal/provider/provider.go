package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/quota"
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
	Name          string
	BaseURL       string
	Models        []string
	Timeout       time.Duration
	Transport     http.RoundTripper
	StreamTimeout time.Duration
}

type Credentials struct {
	APIKey       string
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
	ProjectID    string
	AccountID    string
	UserID       string
	Email        string
}

func (c Credentials) Identity() string {
	if c.UserID != "" && c.AccountID != "" && c.UserID != c.AccountID {
		return c.AccountID + ":" + c.UserID
	}
	if c.UserID != "" {
		return c.UserID
	}
	if c.AccountID != "" && c.Email != "" && c.AccountID != c.Email {
		return c.AccountID + ":" + c.Email
	}
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

// QuotaFetcher is implemented by providers whose upstream exposes remaining
// quota for a credential. Providers without such an endpoint (for example
// openai with a plain API key) simply do not implement it, and the syncer
// skips them.
type QuotaFetcher interface {
	FetchQuota(ctx context.Context, account Credentials) (quota.AccountSnapshot, error)
}

// QuotaHeaderSource is implemented by providers that also observe quota on
// ordinary inference responses. It lets the syncer refresh a snapshot from
// live traffic without waiting for the next poll. ok is false when the
// response carried no quota metadata at all.
type QuotaHeaderSource interface {
	QuotaFromHeaders(account Credentials, h http.Header) (quota.AccountSnapshot, bool)
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
	switch e.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		return false
	}
	return true
}

func (e *HTTPStatusError) DrainReason() string {
	return e.Error()
}

func NewHTTPStatusError(code int, msg string) *HTTPStatusError {
	return &HTTPStatusError{StatusCode: code, Message: msg}
}
