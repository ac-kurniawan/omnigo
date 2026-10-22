// Package systemone is a client for TypeSafe's System One models.
//
// System One is a class of models that make fast, structured decisions: you
// send a state and a set of typed questions, and get back typed answers with
// probabilities instead of generated text. Jev is the first model in the
// class; every model is served by the same endpoint, POST /v1/systemone, and
// the request's model field selects which one handles the call.
//
// This package speaks the protocol only. It does not know what the answers
// mean — deciding what to do with a choice, or which model to name, is the
// caller's business.
package systemone

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
)

// Endpoint is the base URL of the TypeSafe API. The request path is appended
// by the client, so this value must not carry it.
const Endpoint = "https://api.typesafe.ai"

// DefaultModel is the model used when Config.Model is empty. It is an alias
// that follows the newest release, which is the right default for a router
// that wants the current best judgment without pinning a version.
const DefaultModel = "jev-latest"

// Question types. The type selects the answer shape that comes back.
const (
	TypeChoice = "choice"
	TypeScore  = "score"
	TypeNoul   = "noul"
)

// Failures the caller is expected to handle. Every one of them means the same
// thing to a router — no usable answer — so callers can treat them together
// with errors.Is.
var (
	ErrNoAPIKey        = errors.New("systemone: no API key")
	ErrNoQuestions     = errors.New("systemone: no questions")
	ErrUpstream        = errors.New("systemone: upstream failure")
	ErrInvalidResponse = errors.New("systemone: invalid response")
	// ErrLowConfidence marks an answer that is usable but not sure enough to
	// act on. The caller decides what to do with it.
	ErrLowConfidence = errors.New("systemone: confidence below floor")
)

// Limits applied when Config leaves them unset.
const (
	defaultTimeout       = 4 * time.Second
	defaultMaxStateChars = 32000
)

// Question is one judgment to make about the state.
//
// Criteria carries the answer space and its meaning differs by type: a map of
// option name to description for a choice, an ordered list of levels for a
// score, and an optional clarification of yes and no for a noul. Leaving it
// nil is valid for a noul.
type Question struct {
	ID           string
	Type         string
	Instructions string
	Criteria     any
}

// Answer is the typed reply to one Question. Only the fields that belong to
// the question's type are populated; Confidence is set for choice and score
// answers, not for noul.
type Answer struct {
	Type          string
	Choice        string
	Score         float64
	Noul          float64
	Confidence    float64
	Probabilities map[string]float64
	Legend        map[string]string
}

// Model is one System One model the account may call.
type Model struct {
	// Name is the model id or alias to send in the request's model field.
	Name string `json:"name"`
	// Description is the human-readable summary the API returns.
	Description string `json:"description"`
}

// Config configures a Client. Only APIKey is required.
type Config struct {
	// BaseURL overrides Endpoint. Useful for tests and proxies.
	BaseURL string
	// APIKey authenticates the call. An empty key disables the client.
	APIKey string
	// Model selects the System One model, by id or alias. Empty means
	// DefaultModel.
	Model string
	// Timeout bounds one call. Empty means defaultTimeout.
	Timeout time.Duration
	// MaxStateChars truncates the state before sending. Zero means
	// defaultMaxStateChars.
	MaxStateChars int
	// HTTPClient overrides the client used for calls.
	HTTPClient *http.Client
}

// Client calls System One models. It is safe for concurrent use.
type Client struct {
	baseURL       string
	apiKey        string
	model         string
	http          *http.Client
	maxStateChars int
}

// New returns a Client for cfg.
func New(cfg Config) *Client {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = Endpoint
	}
	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxStateChars := cfg.MaxStateChars
	if maxStateChars <= 0 {
		maxStateChars = defaultMaxStateChars
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	return &Client{
		baseURL:       baseURL,
		apiKey:        cfg.APIKey,
		model:         model,
		http:          httpClient,
		maxStateChars: maxStateChars,
	}
}

// Enabled reports whether the client has a key and can be asked anything. A
// disabled client still returns ErrNoAPIKey from Ask, so the caller's fallback
// path is identical whether the key is missing or the call failed.
func (c *Client) Enabled() bool { return c.apiKey != "" }

// Model reports the model id or alias this client sends.
func (c *Client) Model() string { return c.model }

// Truncated reports whether state would be cut to the configured limit. The
// caller can use it to log that a judgment was made on partial input.
func (c *Client) Truncated(state string) bool { return len(state) > c.maxStateChars }

// request is the documented POST /v1/systemone body.
type request struct {
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Questions map[string]question `json:"questions"`
}

type question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type response struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type rawAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Noul          float64            `json:"noul"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Legend        map[string]string  `json:"legend"`
}

// Ask sends every question in one call and returns the answers keyed by
// question id.
//
// All questions see the same state and are evaluated independently, which is
// why several questions belong in one request rather than one request each.
// On failure the error is one of this package's sentinels.
func (c *Client) Ask(ctx context.Context, state string, questions []Question) (map[string]Answer, error) {
	if len(questions) == 0 {
		return nil, ErrNoQuestions
	}
	if !c.Enabled() {
		return nil, ErrNoAPIKey
	}

	trimmed := state
	if len(trimmed) > c.maxStateChars {
		trimmed = trimmed[:c.maxStateChars]
	}

	specs := make(map[string]question, len(questions))
	for _, q := range questions {
		specs[q.ID] = question{
			Type:         q.Type,
			Instructions: q.Instructions,
			Criteria:     q.Criteria,
		}
	}

	raw, err := json.Marshal(request{Model: c.model, State: trimmed, Questions: specs})
	if err != nil {
		return nil, fmt.Errorf("%w: encode request: %v", ErrInvalidResponse, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/systemone", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	// Bound the read so a misbehaving upstream cannot stream unbounded JSON.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrUpstream, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%w: status %d: %s", ErrUpstream, resp.StatusCode, snippet(payload))
	}

	var parsed response
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, fmt.Errorf("%w: decode body: %v", ErrInvalidResponse, err)
	}

	answers := make(map[string]Answer, len(questions))
	for _, q := range questions {
		rawAnswerJSON, ok := parsed.Answers[q.ID]
		if !ok {
			return nil, fmt.Errorf("%w: no answer for question %q", ErrInvalidResponse, q.ID)
		}
		var parsedAnswer rawAnswer
		if err := json.Unmarshal(rawAnswerJSON, &parsedAnswer); err != nil {
			return nil, fmt.Errorf("%w: decode answer %q: %v", ErrInvalidResponse, q.ID, err)
		}
		if parsedAnswer.Type != q.Type {
			return nil, fmt.Errorf("%w: answer %q is %q, want %q", ErrInvalidResponse, q.ID, parsedAnswer.Type, q.Type)
		}
		answers[q.ID] = Answer{
			Type:          parsedAnswer.Type,
			Choice:        parsedAnswer.Choice,
			Score:         parsedAnswer.Score,
			Noul:          parsedAnswer.Noul,
			Confidence:    parsedAnswer.Confidence,
			Probabilities: parsedAnswer.Probabilities,
			Legend:        parsedAnswer.Legend,
		}
	}
	return answers, nil
}

// ListModels reports the System One models this account may call, so a caller
// can offer the models that exist rather than hard-coding names.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	if !c.Enabled() {
		return nil, ErrNoAPIKey
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrUpstream, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%w: status %d: %s", ErrUpstream, resp.StatusCode, snippet(payload))
	}

	var parsed struct {
		Models []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"models"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, fmt.Errorf("%w: decode body: %v", ErrInvalidResponse, err)
	}
	models := make([]Model, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		models = append(models, Model{Name: m.Name, Description: m.Description})
	}
	return models, nil
}

// snippet returns a short, single-line excerpt of an upstream error body for
// the error message. It is deliberately lossy: bodies can be large and the
// message only needs to identify the failure.
func snippet(b []byte) string {
	const limit = 200
	s := strings.TrimSpace(string(b))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		s = s[:limit] + "..."
	}
	return s
}
