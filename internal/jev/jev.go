// Package jev asks TypeSafe's Jev (System One) model which route a request
// should take. It is the decision half of OmniGo's smart combo strategy: it
// never proxies a generation, it only returns the name of one route.
//
// The contract is fail-open. Every failure mode — missing key, HTTP error,
// timeout, malformed payload, an answer outside the route set, or a confidence
// below the caller's floor — is reported as an error, and the caller is
// expected to fall back to its configured default route. Decide never panics
// and never blocks longer than the configured timeout.
package jev

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

// Endpoint is TypeSafe's System One base URL. Every Jev model is served by
// the same path under it; the request's model field selects which one answers.
const Endpoint = "https://api.typesafe.ai"

// Defaults chosen to keep a decision cheap and fast: Jev bills input tokens
// only, so a small state keeps the per-request cost negligible.
const (
	DefaultModel         = "jev-latest"
	DefaultTimeout       = 4 * time.Second
	DefaultMinConfidence = 0.5
	DefaultMaxStateChars = 16000
	DefaultQuestion      = "Which model should handle this request?"
)

// Fail-open sentinels. Callers test these with errors.Is to decide whether a
// decision is worth logging; all of them mean "use the default route".
var (
	// ErrNoAPIKey means TYPESAFE_API_KEY was not set, so no call was made.
	ErrNoAPIKey = errors.New("jev: TYPESAFE_API_KEY is not set")
	// ErrUpstream covers transport failures, timeouts, and non-2xx responses.
	ErrUpstream = errors.New("jev: upstream request failed")
	// ErrInvalidResponse covers a body that is not the documented envelope.
	ErrInvalidResponse = errors.New("jev: invalid response")
	// ErrUnknownRoute means Jev chose an option that is not one of the routes.
	ErrUnknownRoute = errors.New("jev: answer is not a configured route")
	// ErrLowConfidence means the chosen route scored below MinConfidence.
	ErrLowConfidence = errors.New("jev: confidence below the configured floor")
	// ErrNoRoutes means the combo declared no routes to choose between.
	ErrNoRoutes = errors.New("jev: no routes to choose between")
)

// Route is one option Jev chooses between: a name, the criteria that describe
// when it applies, and (for the caller) where it leads. The destination is not
// part of the request sent to Jev; only name and criteria are.
type Route struct {
	Name     string
	Criteria string
}

// Config configures a Client. Zero values fall back to the defaults above, so
// a caller that only sets APIKey gets a working client.
type Config struct {
	BaseURL       string
	Model         string
	APIKey        string
	Timeout       time.Duration
	MinConfidence float64
	Question      string
	// MaxStateChars bounds the state sent to Jev. Jev's per-request context is
	// 64k tokens; this keeps a pathological prompt from being rejected, and
	// keeps the billed input small.
	MaxStateChars int
	// HTTPClient overrides the transport, used by tests.
	HTTPClient *http.Client
}

// Decision is what Jev answered, plus the bookkeeping the caller needs to log
// and to apply its own fallback policy.
type Decision struct {
	Route      string
	Confidence float64
	Model      string
	// Truncated reports that the state was trimmed to MaxStateChars.
	Truncated bool
	// InputTokens is Jev's reported usage, for cost accounting.
	InputTokens int
}

// Client asks one Jev question per call. It is safe for concurrent use.
type Client struct {
	baseURL       string
	model         string
	apiKey        string
	minConfidence float64
	question      string
	maxStateChars int
	http          *http.Client
}

// New builds a Client from cfg, applying defaults for anything unset.
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
		timeout = DefaultTimeout
	}
	question := cfg.Question
	if question == "" {
		question = DefaultQuestion
	}
	maxStateChars := cfg.MaxStateChars
	if maxStateChars <= 0 {
		maxStateChars = DefaultMaxStateChars
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	return &Client{
		baseURL:       baseURL,
		model:         model,
		apiKey:        cfg.APIKey,
		minConfidence: cfg.MinConfidence,
		question:      question,
		maxStateChars: maxStateChars,
		http:          httpClient,
	}
}

// Enabled reports whether the client has a key and can be asked anything. A
// disabled client still returns ErrNoAPIKey from Decide, so the caller's
// fallback path is identical whether the key is missing or the call failed.
func (c *Client) Enabled() bool { return c.apiKey != "" }

// questionName is the key the answer comes back under. It is fixed so the
// response parse does not depend on the caller's wording.
const questionName = "route"

// systemOneRequest is the documented POST /v1/systemone body for a choice
// question. Only the fields OmniGo sends are modelled.
type systemOneRequest struct {
	Model     string                    `json:"model"`
	State     string                    `json:"state"`
	Questions map[string]systemOneQSpec `json:"questions"`
}

type systemOneQSpec struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type systemOneResponse struct {
	Model   string                    `json:"model"`
	Answers map[string]systemOneReply `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type systemOneReply struct {
	Type       string  `json:"type"`
	Choice     string  `json:"choice"`
	Confidence float64 `json:"confidence"`
}

// Decide asks Jev which of routes applies to state and returns its answer.
//
// state is the text Jev judges — OmniGo passes the last user message. On
// success the returned Decision names one of routes. On any failure the error
// is one of this package's sentinels and the Decision carries whatever partial
// information was learned (so a low-confidence answer can still be logged).
func (c *Client) Decide(ctx context.Context, state string, routes []Route) (Decision, error) {
	if len(routes) == 0 {
		return Decision{}, ErrNoRoutes
	}
	if !c.Enabled() {
		return Decision{}, ErrNoAPIKey
	}

	trimmed := state
	truncated := false
	if len(trimmed) > c.maxStateChars {
		trimmed = trimmed[:c.maxStateChars]
		truncated = true
	}

	criteria := make(map[string]string, len(routes))
	for _, route := range routes {
		criteria[route.Name] = route.Criteria
	}
	body := systemOneRequest{
		Model: c.model,
		State: trimmed,
		Questions: map[string]systemOneQSpec{
			questionName: {
				Type:         "choice",
				Instructions: c.question,
				Criteria:     criteria,
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: encode request: %v", ErrInvalidResponse, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/systemone", bytes.NewReader(raw))
	if err != nil {
		return Decision{}, fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Decision{Truncated: truncated}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	// Bound the read so a misbehaving upstream cannot stream unbounded JSON.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Decision{Truncated: truncated}, fmt.Errorf("%w: read body: %v", ErrUpstream, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Decision{Truncated: truncated}, fmt.Errorf("%w: status %d: %s", ErrUpstream, resp.StatusCode, snippet(payload))
	}

	var parsed systemOneResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return Decision{Truncated: truncated}, fmt.Errorf("%w: decode body: %v", ErrInvalidResponse, err)
	}
	reply, ok := parsed.Answers[questionName]
	if !ok {
		return Decision{Truncated: truncated}, fmt.Errorf("%w: no answer for question %q", ErrInvalidResponse, questionName)
	}
	if reply.Type != "choice" || reply.Choice == "" {
		return Decision{Truncated: truncated}, fmt.Errorf("%w: answer is not a choice", ErrInvalidResponse)
	}

	decision := Decision{
		Route:       reply.Choice,
		Confidence:  reply.Confidence,
		Model:       parsed.Model,
		Truncated:   truncated,
		InputTokens: parsed.Usage.InputTokens,
	}

	if !knownRoute(routes, reply.Choice) {
		return decision, fmt.Errorf("%w: %q", ErrUnknownRoute, reply.Choice)
	}
	if decision.Confidence < c.minConfidence {
		return decision, fmt.Errorf("%w: %s at %.2f (floor %.2f)", ErrLowConfidence, decision.Route, decision.Confidence, c.minConfidence)
	}
	return decision, nil
}

func knownRoute(routes []Route, name string) bool {
	for _, route := range routes {
		if route.Name == name {
			return true
		}
	}
	return false
}

// snippet keeps an upstream error body short enough to log safely.
func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
