package muse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type memoryCredStore struct {
	creds provider.Credentials
}

func (m *memoryCredStore) Get() provider.Credentials {
	return m.creds
}

func (m *memoryCredStore) Put(c provider.Credentials) error {
	m.creds = c
	return nil
}

func TestMuseProviderChatCompletionAttachesHeaders(t *testing.T) {
	var capturedHeader http.Header
	var capturedBody []byte

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Clone()
		capturedBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chatcmpl-123","choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	defer ts.Close()

	store := &memoryCredStore{
		creds: provider.Credentials{
			APIKey: "LLM|test-key",
			Email:  "user@example.com",
		},
	}

	p := New(provider.Config{
		Name:    "meta-muse",
		BaseURL: ts.URL,
		Timeout: 5 * time.Second,
	}, store)

	rec := httptest.NewRecorder()
	req := provider.ChatRequest{
		Model: "muse-spark-1.3",
		Raw:   []byte(`{"model":"muse-spark-1.3","messages":[{"role":"user","content":"hi"}]}`),
	}

	err := p.ChatCompletion(context.Background(), req, rec)
	if err != nil {
		t.Fatalf("ChatCompletion failed: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	if capturedHeader.Get("Authorization") != "Bearer LLM|test-key" {
		t.Errorf("expected Authorization Bearer LLM|test-key, got %s", capturedHeader.Get("Authorization"))
	}
	if capturedHeader.Get("x-api-version") != "1.0.0" {
		t.Errorf("expected x-api-version 1.0.0, got %s", capturedHeader.Get("x-api-version"))
	}
	if len(capturedBody) == 0 {
		t.Errorf("expected request body forwarded")
	}
}

func TestMuseProviderModelsDiscovery(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-version") != "1.0.0" {
			t.Errorf("expected x-api-version 1.0.0 on models request")
		}
		if r.Header.Get("Authorization") != "Bearer LLM|test-key" {
			t.Errorf("expected Authorization Bearer LLM|test-key")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "muse-spark-1.3"},
				{"id": "muse-spark-1.2"},
			},
		})
	}))
	defer ts.Close()

	store := &memoryCredStore{
		creds: provider.Credentials{
			APIKey: "LLM|test-key",
		},
	}

	p := New(provider.Config{
		Name:    "meta-muse",
		BaseURL: ts.URL,
	}, store)

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models failed: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "muse-spark-1.3" {
		t.Errorf("expected muse-spark-1.3, got %s", models[0].ID)
	}

	testRes := p.Test(context.Background())
	if !testRes.OK {
		t.Errorf("expected Test to succeed: %v", testRes.Error)
	}
}
