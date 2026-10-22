package systemone

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

// The models the account can use are returned with the field names the
// dashboard's JavaScript reads, so the picker is not fed Go field names.
func TestListModelsEncodesLowercaseJSONFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"jev-latest","description":"flagship"}]}`))
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, APIKey: "test-key"})
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	raw, err := json.Marshal(models)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"name":"jev-latest"`) {
		t.Errorf("encoded models = %s, want a lowercase \"name\" field", raw)
	}
	if !strings.Contains(string(raw), `"description":"flagship"`) {
		t.Errorf("encoded models = %s, want a lowercase \"description\" field", raw)
	}
}

// The client posts to the documented System One path exactly once. A base URL
// that already carries the path must not have it appended again, or every
// decision 404s.
func TestAskPostsToSystemOnePathOnce(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"q":{"type":"choice","choice":"a","confidence":0.9}},"usage":{"input_tokens":10,"output_tokens":2}}`))
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, APIKey: "test-key", Model: "jev-latest"})
	answers, err := client.Ask(context.Background(), "some state", []Question{{
		ID:           "q",
		Type:         TypeChoice,
		Instructions: "which one?",
		Criteria:     map[string]string{"a": "first", "b": "second"},
	}})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/systemone" {
		t.Errorf("path = %q, want /v1/systemone", gotPath)
	}
	if len(answers) != 1 {
		t.Fatalf("answers = %d, want 1", len(answers))
	}
	if answers["q"].Choice != "a" {
		t.Errorf("choice = %q, want a", answers["q"].Choice)
	}
}

// The default endpoint is a base URL: the path is appended by the client.
func TestDefaultEndpointIsBaseURL(t *testing.T) {
	if strings.Contains(Endpoint, "/v1/systemone") {
		t.Fatalf("Endpoint = %q must not carry the request path", Endpoint)
	}
	if Endpoint != "https://api.typesafe.ai" {
		t.Errorf("Endpoint = %q", Endpoint)
	}
}

// The request body carries the documented envelope: model, state, and the
// questions keyed by the id the caller chose.
func TestAskSendsDocumentedEnvelope(t *testing.T) {
	var body struct {
		Model     string                     `json:"model"`
		State     string                     `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"q":{"type":"choice","choice":"a"}}}`))
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "jev-preview"})
	_, err := client.Ask(context.Background(), "the state", []Question{{
		ID:           "q",
		Type:         TypeChoice,
		Instructions: "pick",
		Criteria:     map[string]string{"a": "A", "b": "B"},
	}})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if body.Model != "jev-preview" {
		t.Errorf("model = %q, want jev-preview", body.Model)
	}
	if body.State != "the state" {
		t.Errorf("state = %q", body.State)
	}
	raw := string(body.Questions["q"])
	if !strings.Contains(raw, `"type":"choice"`) {
		t.Errorf("question = %s, want type choice", raw)
	}
	if !strings.Contains(raw, `"instructions":"pick"`) {
		t.Errorf("question = %s, want instructions", raw)
	}
	if !strings.Contains(raw, `"a":"A"`) {
		t.Errorf("question = %s, want criteria", raw)
	}
}

// The authorization header carries the key.
func TestAskSendsBearerToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q":{"type":"noul","noul":0.75}}}`))
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, APIKey: "secret-key", Model: "jev-latest"})
	_, err := client.Ask(context.Background(), "state", []Question{{ID: "q", Type: TypeNoul, Instructions: "is it?"}})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "Bearer secret-key" {
		t.Errorf("Authorization = %q", got)
	}
}

// Every question in one request sees the same state, and the answers come
// back keyed by the question ids. This is the fan-out the smart router uses
// to ask several questions in a single call.
func TestAskSupportsSeveralQuestionsInOneCall(t *testing.T) {
	var questionCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.Unmarshal(raw, &body)
		questionCount = len(body.Questions)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"a":{"type":"choice","choice":"x","confidence":0.8},"b":{"type":"noul","noul":0.9}}}`))
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "jev-latest"})
	answers, err := client.Ask(context.Background(), "state", []Question{
		{ID: "a", Type: TypeChoice, Instructions: "which?", Criteria: map[string]string{"x": "X", "y": "Y"}},
		{ID: "b", Type: TypeNoul, Instructions: "is it?"},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if questionCount != 2 {
		t.Errorf("questions sent = %d, want 2", questionCount)
	}
	if answers["a"].Choice != "x" || answers["a"].Confidence != 0.8 {
		t.Errorf("answer a = %+v", answers["a"])
	}
	if answers["b"].Noul != 0.9 {
		t.Errorf("answer b = %+v", answers["b"])
	}
}

// A score answer carries the level and the distribution across levels.
func TestAskParsesScoreAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q":{"type":"score","score":1.7,"confidence":0.55,"legend":{"0":"low","1":"mid"},"probabilities":{"0":0.1,"1":0.9}}}}`))
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "jev-latest"})
	answers, err := client.Ask(context.Background(), "state", []Question{{
		ID: "q", Type: TypeScore, Instructions: "how much?",
		Criteria: []string{"low", "mid"},
	}})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	answer := answers["q"]
	if answer.Score != 1.7 {
		t.Errorf("score = %v, want 1.7", answer.Score)
	}
	if answer.Confidence != 0.55 {
		t.Errorf("confidence = %v", answer.Confidence)
	}
	if answer.Legend["1"] != "mid" {
		t.Errorf("legend = %v", answer.Legend)
	}
	if answer.Probabilities["1"] != 0.9 {
		t.Errorf("probabilities = %v", answer.Probabilities)
	}
}

// Every failure mode is one of the package's sentinels, so the caller can
// treat them all the same way.
func TestAskFailures(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    error
	}{
		{
			name: "http 500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"detail":"boom"}`))
			},
			want: ErrUpstream,
		},
		{
			name: "http 429",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
			},
			want: ErrUpstream,
		},
		{
			name: "malformed json",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`not json`))
			},
			want: ErrInvalidResponse,
		},
		{
			name: "answers missing",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"model":"x"}`))
			},
			want: ErrInvalidResponse,
		},
		{
			name: "answer type mismatch",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"answers":{"q":{"type":"noul","noul":0.5}}}`))
			},
			want: ErrInvalidResponse,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			client := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "jev-latest"})
			_, err := client.Ask(context.Background(), "state", []Question{{
				ID: "q", Type: TypeChoice, Instructions: "which?",
				Criteria: map[string]string{"a": "A"},
			}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// A missing key fails the same way as an upstream error, so the caller has
// one fallback path.
func TestAskWithoutAPIKey(t *testing.T) {
	client := New(Config{BaseURL: "http://127.0.0.1:1", Model: "jev-latest"})
	_, err := client.Ask(context.Background(), "state", []Question{{ID: "q", Type: TypeChoice, Instructions: "x"}})
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("err = %v, want ErrNoAPIKey", err)
	}
}

// A slow upstream is abandoned at the configured timeout.
func TestAskTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q":{"type":"choice","choice":"a"}}}`))
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "jev-latest", Timeout: 50 * time.Millisecond})
	_, err := client.Ask(context.Background(), "state", []Question{{ID: "q", Type: TypeChoice, Instructions: "x"}})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
}

// Listing models reports the names the account may send, so the dashboard can
// offer the models that exist instead of guessing.
func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"jev-latest","description":"latest"},{"name":"jev-preview","description":"preview"}]}`))
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "jev-latest"})
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
	if models[0].Name != "jev-latest" || models[1].Name != "jev-preview" {
		t.Errorf("models = %+v", models)
	}
	if models[0].Description != "latest" {
		t.Errorf("description = %q", models[0].Description)
	}
}

func TestListModelsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	client := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "jev-latest"})
	if _, err := client.ListModels(context.Background()); !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
}

// The state is truncated to the configured limit so a long prompt cannot
// blow past the model context.
func TestAskTruncatesState(t *testing.T) {
	var gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(raw, &body)
		gotState = body.State
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"q":{"type":"choice","choice":"a"}}}`))
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "jev-latest", MaxStateChars: 10})
	if _, err := client.Ask(context.Background(), strings.Repeat("x", 100), []Question{{ID: "q", Type: TypeChoice, Instructions: "x"}}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(gotState) != 10 {
		t.Errorf("state len = %d, want 10", len(gotState))
	}
}

// A request with no questions is a caller error, not an upstream call.
func TestAskRejectsEmptyQuestions(t *testing.T) {
	client := New(Config{BaseURL: "http://127.0.0.1:1", APIKey: "k", Model: "jev-latest"})
	if _, err := client.Ask(context.Background(), "state", nil); !errors.Is(err, ErrNoQuestions) {
		t.Fatalf("err = %v, want ErrNoQuestions", err)
	}
}
