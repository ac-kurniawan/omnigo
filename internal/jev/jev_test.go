package jev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// systemOneChoiceResponse is the exact envelope TypeSafe returns for a choice
// question (verified against the live API): the answers map is keyed by the
// question name the caller chose, and the choice value is the criteria key.
func systemOneChoiceResponse(question, choice string, confidence float64) string {
	body := map[string]any{
		"model": "jev-1.13.0",
		"answers": map[string]any{
			question: map[string]any{
				"type":       "choice",
				"choice":     choice,
				"confidence": confidence,
				"probabilities": map[string]any{
					choice: confidence,
				},
			},
		},
		"usage": map[string]any{"input_tokens": 120, "output_tokens": 12},
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func testRoutes() []Route {
	return []Route{
		{Name: "flash", Criteria: "A simple request that needs a fast answer."},
		{Name: "pro", Criteria: "A complex request needing deep reasoning."},
	}
}

func TestDecideSelectsRouteFromChoice(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(systemOneChoiceResponse("route", "pro", 0.91)))
	}))
	defer srv.Close()

	c := New(Config{
		BaseURL:       srv.URL,
		Model:         "jev-latest",
		APIKey:        "test-key",
		Timeout:       2 * time.Second,
		MinConfidence: 0.5,
		Question:      "Which model should handle this request?",
	})
	got, err := c.Decide(context.Background(), "refactor the auth middleware", testRoutes())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Route != "pro" {
		t.Errorf("route = %q, want pro", got.Route)
	}
	if got.Confidence != 0.91 {
		t.Errorf("confidence = %v, want 0.91", got.Confidence)
	}
	if gotPath != "/v1/systemone" {
		t.Errorf("path = %q, want /v1/systemone", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotBody["model"] != "jev-latest" {
		t.Errorf("model = %v", gotBody["model"])
	}
	if gotBody["state"] != "refactor the auth middleware" {
		t.Errorf("state = %v", gotBody["state"])
	}

	// The question must be a choice whose criteria keys are exactly the route
	// names, with each route's criteria text as the option description.
	questions, ok := gotBody["questions"].(map[string]any)
	if !ok || len(questions) != 1 {
		t.Fatalf("questions = %#v", gotBody["questions"])
	}
	q, ok := questions["route"].(map[string]any)
	if !ok {
		t.Fatalf("question route = %#v", questions["route"])
	}
	if q["type"] != "choice" {
		t.Errorf("question type = %v, want choice", q["type"])
	}
	criteria, ok := q["criteria"].(map[string]any)
	if !ok {
		t.Fatalf("criteria = %#v", q["criteria"])
	}
	if len(criteria) != 2 {
		t.Errorf("criteria len = %d, want 2", len(criteria))
	}
	if criteria["flash"] != "A simple request that needs a fast answer." {
		t.Errorf("criteria[flash] = %v", criteria["flash"])
	}
	if criteria["pro"] != "A complex request needing deep reasoning." {
		t.Errorf("criteria[pro] = %v", criteria["pro"])
	}
}

func TestDecideLowConfidenceReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(systemOneChoiceResponse("route", "pro", 0.2)))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Model: "jev-latest", APIKey: "k", Timeout: time.Second, MinConfidence: 0.5})
	got, err := c.Decide(context.Background(), "hello", testRoutes())
	if !errors.Is(err, ErrLowConfidence) {
		t.Fatalf("err = %v, want ErrLowConfidence", err)
	}
	// The decision is still reported so the caller can log what Jev said.
	if got.Route != "pro" || got.Confidence != 0.2 {
		t.Errorf("decision = %+v, want pro/0.2", got)
	}
}

func TestDecideUnknownChoiceReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(systemOneChoiceResponse("route", "not_a_route", 0.99)))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Model: "jev-latest", APIKey: "k", Timeout: time.Second, MinConfidence: 0.5})
	_, err := c.Decide(context.Background(), "hello", testRoutes())
	if !errors.Is(err, ErrUnknownRoute) {
		t.Fatalf("err = %v, want ErrUnknownRoute", err)
	}
}

// Fail-open matrix: every one of these must return an error and never panic,
// so the caller can fall back to its configured default route.
func TestDecideFailOpen(t *testing.T) {
	tests := []struct {
		name    string
		apiKey  string
		handler http.HandlerFunc
		timeout time.Duration
		want    error
	}{
		{
			name:   "missing api key",
			apiKey: "",
			handler: func(w http.ResponseWriter, r *http.Request) {
				t.Error("upstream must not be called without a key")
			},
			timeout: time.Second,
			want:    ErrNoAPIKey,
		},
		{
			name:   "http 500",
			apiKey: "k",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"detail":"boom"}`))
			},
			timeout: time.Second,
			want:    ErrUpstream,
		},
		{
			name:   "http 429",
			apiKey: "k",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
			},
			timeout: time.Second,
			want:    ErrUpstream,
		},
		{
			name:   "malformed json",
			apiKey: "k",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{not json`))
			},
			timeout: time.Second,
			want:    ErrInvalidResponse,
		},
		{
			name:   "answers missing",
			apiKey: "k",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"model":"jev-1.13.0","usage":{}}`))
			},
			timeout: time.Second,
			want:    ErrInvalidResponse,
		},
		{
			name:   "choice wrong type",
			apiKey: "k",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"answers":{"route":{"type":"choice","choice":42}}}`))
			},
			timeout: time.Second,
			want:    ErrInvalidResponse,
		},
		{
			name:   "timeout",
			apiKey: "k",
			handler: func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(300 * time.Millisecond)
				_, _ = w.Write([]byte(systemOneChoiceResponse("route", "pro", 0.9)))
			},
			timeout: 50 * time.Millisecond,
			want:    ErrUpstream,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c := New(Config{BaseURL: srv.URL, Model: "jev-latest", APIKey: tc.apiKey, Timeout: tc.timeout, MinConfidence: 0.5})
			_, err := c.Decide(context.Background(), "hello", testRoutes())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDecideNoRoutesReturnsError(t *testing.T) {
	c := New(Config{BaseURL: "http://127.0.0.1:1", Model: "jev-latest", APIKey: "k", Timeout: time.Second})
	_, err := c.Decide(context.Background(), "hello", nil)
	if !errors.Is(err, ErrNoRoutes) {
		t.Fatalf("err = %v, want ErrNoRoutes", err)
	}
}

// Jev's per-request context is 64k tokens; a runaway prompt must be trimmed
// rather than rejected, and the trim must be visible to the caller.
func TestDecideTruncatesLongState(t *testing.T) {
	var gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		gotState, _ = body["state"].(string)
		_, _ = w.Write([]byte(systemOneChoiceResponse("route", "flash", 0.8)))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Model: "jev-latest", APIKey: "k", Timeout: time.Second, MinConfidence: 0.5, MaxStateChars: 100})
	long := strings.Repeat("x", 5000)
	got, err := c.Decide(context.Background(), long, testRoutes())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(gotState) > 100 {
		t.Errorf("state len = %d, want <= 100", len(gotState))
	}
	if !got.Truncated {
		t.Error("Truncated = false, want true")
	}
}

func TestDecideDefaultMaxStateChars(t *testing.T) {
	c := New(Config{Model: "jev-latest", APIKey: "k"})
	if c.maxStateChars <= 0 {
		t.Fatalf("maxStateChars = %d, want a positive default", c.maxStateChars)
	}
}
