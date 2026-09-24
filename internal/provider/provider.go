package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	// Parsed is the one JSON decode of Raw. It is shared across targets and
	// must not be mutated by callers.
	Parsed map[string]any
}

const openAIBodyCacheKey = "\x00omnigo-openai-body-cache"

// OpenAIBodyCacheKey is the internal parsed-map slot holding encoded payloads.
// Translators that range over Parsed must ignore it.
func OpenAIBodyCacheKey() string { return openAIBodyCacheKey }

// ParseBody decodes a chat request once. Invalid JSON returns nil so callers
// can preserve the existing fallback to Raw or the typed message fields.
func ParseBody(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var parsed map[string]any
	if err := dec.Decode(&parsed); err != nil || parsed == nil {
		return nil
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil
	}
	parsed[openAIBodyCacheKey] = &openAIBodyCache{bodies: make(map[string][]byte)}
	return parsed
}

type openAIBodyCache struct {
	bodies map[string][]byte
}

// Body returns the request JSON payload intended for upstream forwarding,
// ensuring the "model" field reflects req.Model (the resolved bare model id)
// and normalizes "developer" role messages to "system" for broad provider compatibility.
// A parsed body is encoded once per model. The cache is attached to the shared
// parsed map, so value copies of the request reuse the same bytes.
func (r ChatRequest) Body() ([]byte, error) {
	if cache := openAICache(r.Parsed); cache != nil {
		if body, ok := cache.bodies[r.Model]; ok {
			return body, nil
		}
	}
	body, err := encodeChatBody(r)
	if err != nil || len(body) == 0 {
		return body, err
	}
	if cache := openAICache(r.Parsed); cache != nil {
		cache.bodies[r.Model] = body
	}
	return body, nil
}

func openAICache(parsed map[string]any) *openAIBodyCache {
	if parsed == nil {
		return nil
	}
	cache, _ := parsed[openAIBodyCacheKey].(*openAIBodyCache)
	return cache
}

func encodeChatBody(r ChatRequest) ([]byte, error) {
	if r.Parsed != nil {
		return json.Marshal(openAIPayload(r.Parsed, r.Model))
	}
	if len(r.Raw) > 0 {
		parsed := ParseBody(r.Raw)
		if parsed != nil {
			return json.Marshal(openAIPayload(parsed, r.Model))
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

func openAIPayload(parsed map[string]any, model string) map[string]any {
	out := make(map[string]any, len(parsed)-1)
	for key, value := range parsed {
		if key == openAIBodyCacheKey {
			continue
		}
		out[key] = value
	}
	out["model"] = model
	rawMessages, ok := out["messages"].([]any)
	if !ok {
		return out
	}
	messages := make([]any, len(rawMessages))
	copy(messages, rawMessages)
	for i, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, ok := message["role"].(string)
		if !ok || role != "developer" {
			continue
		}
		copied := make(map[string]any, len(message))
		for key, value := range message {
			copied[key] = value
		}
		copied["role"] = "system"
		messages[i] = copied
	}
	out["messages"] = messages
	return out
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
	return !isClientError(e)
}

func (e *HTTPStatusError) DrainReason() string {
	return e.Error()
}

func NewHTTPStatusError(code int, msg string) *HTTPStatusError {
	return &HTTPStatusError{StatusCode: code, Message: msg}
}
