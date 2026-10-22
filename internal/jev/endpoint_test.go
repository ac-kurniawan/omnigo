package jev

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The client must post to the documented System One path exactly once. A
// base URL that already carries the path would produce a doubled path and a
// 404 from the real API.
func TestDecidePostsToSystemOnePathOnce(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-latest","answers":{"route":{"type":"choice","choice":"a","confidence":0.9}},"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, APIKey: "k", MinConfidence: 0.5})
	_, err := client.Decide(context.Background(), "state", []Route{{Name: "a", Criteria: "c"}})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if gotPath != "/v1/systemone" {
		t.Fatalf("path = %q, want /v1/systemone", gotPath)
	}
}

// The default endpoint must be the TypeSafe base URL, so the client builds
// the System One path itself.
func TestDefaultEndpointIsBaseURL(t *testing.T) {
	if strings.HasSuffix(Endpoint, "/v1/systemone") {
		t.Fatalf("Endpoint %q must be a base URL: the client appends the path", Endpoint)
	}
	if !strings.HasPrefix(Endpoint, "https://api.typesafe.ai") {
		t.Fatalf("Endpoint = %q, want the TypeSafe host", Endpoint)
	}
}
