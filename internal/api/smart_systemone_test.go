package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/systemone"
)

// newTestSystemOneClient builds a client pointed at a local stand-in server.
func newTestSystemOneClient(baseURL, model string) *systemone.Client {
	return systemone.New(systemone.Config{
		BaseURL: baseURL,
		Model:   model,
		APIKey:  "test-key",
	})
}

// The System One client asks whichever model the combo names, so a combo can
// use any System One model the account has, not only Jev.
func TestSystemOneDeciderUsesTheConfiguredModel(t *testing.T) {
	var gotModel string
	var gotQuestions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string `json:"model"`
			Questions map[string]struct {
				Type string `json:"type"`
			} `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		for id := range body.Questions {
			gotQuestions = append(gotQuestions, id)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"` + body.Model + `","answers":{"route":{"type":"choice","choice":"pro","confidence":0.9}}}`))
	}))
	t.Cleanup(srv.Close)

	client := newTestSystemOneClient(srv.URL, "some-other-system-one-model")
	decider := newSystemOneDecider(client, config.SmartDecision{
		Model:         "some-other-system-one-model",
		MinConfidence: "0.5",
	})

	verdict, err := decider.Decide(context.Background(), "refactor the auth layer", []config.SmartRoute{
		{Name: "flash", Criteria: "simple"},
		{Name: "pro", Criteria: "complex"},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if verdict.Route != "pro" {
		t.Errorf("route = %q, want pro", verdict.Route)
	}
	if gotModel != "some-other-system-one-model" {
		t.Errorf("model sent = %q, want some-other-system-one-model", gotModel)
	}
	if len(gotQuestions) != 1 || gotQuestions[0] != "route" {
		t.Errorf("questions = %v, want [route]", gotQuestions)
	}
}

// The fanout mode asks one question per route in a single call, and the
// highest weighted probability wins.
func TestSystemOneDeciderFanoutCombinesPerRouteAnswers(t *testing.T) {
	var questionCount int
	var questionIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		questionCount = len(body.Questions)
		for id := range body.Questions {
			questionIDs = append(questionIDs, id)
		}
		w.Header().Set("Content-Type", "application/json")
		// flash scores 0.8, pro scores 0.6. With pro weighted 1.5, pro wins
		// at 0.9 against flash at 0.8.
		_, _ = w.Write([]byte(`{"answers":{
			"route_flash":{"type":"noul","noul":0.8},
			"route_pro":{"type":"noul","noul":0.6}
		}}`))
	}))
	t.Cleanup(srv.Close)

	client := newTestSystemOneClient(srv.URL, "jev-latest")
	decider := newSystemOneDecider(client, config.SmartDecision{
		Model:         "jev-latest",
		Mode:          config.DecisionModeFanout,
		MinConfidence: "0.5",
		Weights:       map[string]float64{"flash": 1.0, "pro": 1.5},
	})

	verdict, err := decider.Decide(context.Background(), "refactor the auth layer", []config.SmartRoute{
		{Name: "flash", Criteria: "simple"},
		{Name: "pro", Criteria: "complex"},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if questionCount != 2 {
		t.Errorf("questions asked = %d, want 2", questionCount)
	}
	if len(questionIDs) != 2 {
		t.Errorf("question ids = %v, want two", questionIDs)
	}
	if verdict.Route != "pro" {
		t.Errorf("route = %q, want pro (weighted 0.6*1.5=0.9 beats 0.8)", verdict.Route)
	}
}

// A fanout answer below the confidence floor falls back instead of routing on
// a guess.
func TestSystemOneDeciderFanoutBelowConfidenceFloorFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"route_flash":{"type":"noul","noul":0.2}}}`))
	}))
	t.Cleanup(srv.Close)

	client := newTestSystemOneClient(srv.URL, "jev-latest")
	decider := newSystemOneDecider(client, config.SmartDecision{
		Model:         "jev-latest",
		Mode:          config.DecisionModeFanout,
		MinConfidence: "0.5",
	})

	_, err := decider.Decide(context.Background(), "hello", []config.SmartRoute{
		{Name: "flash", Criteria: "simple"},
	})
	if err == nil {
		t.Fatal("Decide succeeded, want an error below the confidence floor")
	}
}

// A choice answer below the confidence floor also falls back.
func TestSystemOneDeciderBelowConfidenceFloorFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"route":{"type":"choice","choice":"pro","confidence":0.3}}}`))
	}))
	t.Cleanup(srv.Close)

	client := newTestSystemOneClient(srv.URL, "jev-latest")
	decider := newSystemOneDecider(client, config.SmartDecision{
		Model:         "jev-latest",
		MinConfidence: "0.5",
	})

	_, err := decider.Decide(context.Background(), "hello", []config.SmartRoute{
		{Name: "flash", Criteria: "simple"},
		{Name: "pro", Criteria: "complex"},
	})
	if err == nil {
		t.Fatal("Decide succeeded, want an error below the confidence floor")
	}
}
