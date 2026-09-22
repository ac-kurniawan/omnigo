package antigravity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestParseModelsResponseMap(t *testing.T) {
	// Google's actual /v1internal:fetchAvailableModels format
	body := []byte(`{
		"models": {
			"gemini-3.7-flash-medium": {"displayName": "Gemini 3.7 Flash"},
			"claude-sonnet-4-6": {"displayName": "Claude Sonnet 4.6"}
		}
	}`)
	ids := parseModelsResponse(body)
	sort.Strings(ids)
	if len(ids) != 2 {
		t.Fatalf("got %d models, want 2: %v", len(ids), ids)
	}
	if ids[0] != "claude-sonnet-4-6" || ids[1] != "gemini-3.7-flash-medium" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestParseModelsResponseArray(t *testing.T) {
	body := []byte(`{
		"models": [
			{"name": "gemini-3.7-flash-medium"},
			{"name": "gpt-oss-120b"}
		]
	}`)
	ids := parseModelsResponse(body)
	if len(ids) != 2 || ids[0] != "gemini-3.7-flash-medium" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestFetchModelsFallsBackWhenUpstreamEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	baseURL = srv.URL

	ids, err := fetchModels(context.Background(), "tok", "proj-1")
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("expected fallback models on empty upstream response")
	}
	var hasFlash bool
	for _, id := range ids {
		if strings.Contains(id, "gemini-3.7-flash") {
			hasFlash = true
			break
		}
	}
	if !hasFlash {
		t.Fatalf("fallback models missing gemini-3.7-flash: %v", ids)
	}
}

func TestFetchModelsPassesProject(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(`{"models":{"gemini-3.7-flash-medium":{}}}`))
	}))
	defer srv.Close()
	baseURL = srv.URL

	_, err := fetchModels(context.Background(), "tok", "my-gcp-project")
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	if !strings.Contains(gotBody, "my-gcp-project") {
		t.Fatalf("body = %s, want project", gotBody)
	}
}

func TestProviderModelsWithGoogleObjectFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"models": {
				"gemini-3.7-flash-medium": {},
				"claude-sonnet-4-6": {}
			}
		}`))
	}))
	defer srv.Close()

	p := New(provider.Config{Name: "agy", BaseURL: srv.URL}, staticStore{provider.Credentials{AccessToken: "tok", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour)}})
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want 2", models)
	}
}

func TestDefaultBaseURLUsesDailyEndpoint(t *testing.T) {
	want := "https://daily-cloudcode-pa.googleapis.com"
	if defaultBaseURL != want {
		t.Fatalf("defaultBaseURL = %q, want %q", defaultBaseURL, want)
	}
}

func TestNormalizeBaseURLRedirectsLegacyHost(t *testing.T) {
	if got := normalizeBaseURL("https://cloudcode-pa.googleapis.com"); got != defaultBaseURL {
		t.Fatalf("normalizeBaseURL(legacy) = %q, want %q", got, defaultBaseURL)
	}
	if got := normalizeBaseURL("https://cloudcode-pa.googleapis.com/"); got != defaultBaseURL {
		t.Fatalf("normalizeBaseURL(legacy trailing slash) = %q, want %q", got, defaultBaseURL)
	}
	custom := "https://proxy.example.test/v1"
	if got := normalizeBaseURL(custom); got != custom {
		t.Fatalf("normalizeBaseURL(custom) = %q, want passthrough %q", got, custom)
	}
}
