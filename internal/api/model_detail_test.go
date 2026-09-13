package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestModelDetailUnauthorized(t *testing.T) {
	rr := httptest.NewRecorder()
	testRouter(t, &config.Config{}, &vault.Vault{}).ServeHTTP(rr, httptest.NewRequest("GET", "/v1/models/openai/gpt-4o", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestModelDetailDirectProvider(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openai", Type: "openai", BaseURL: "https://x", Models: []string{"gpt-4o"}}},
	}
	body := requestModelDetail(t, cfg, "/v1/models/openai/gpt-4o")
	if body.ID != "openai/gpt-4o" || body.Object != "model" || body.OwnedBy != "openai" {
		t.Fatalf("model = %+v", body)
	}
}

func TestModelDetailCombo(t *testing.T) {
	cfg := &config.Config{
		Combos: []config.Combo{{Name: "auto", Strategy: "priority"}},
	}
	body := requestModelDetail(t, cfg, "/v1/models/auto")
	if body.ID != "auto" || body.Object != "model" || body.OwnedBy != "combo" {
		t.Fatalf("model = %+v", body)
	}
}

func TestModelDetailSupportsSlashesInProviderModelID(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "openrouter", Type: "openai", BaseURL: "https://x", Models: []string{"anthropic/claude-sonnet-4"}}},
	}
	body := requestModelDetail(t, cfg, "/v1/models/openrouter/anthropic/claude-sonnet-4")
	if body.ID != "openrouter/anthropic/claude-sonnet-4" {
		t.Fatalf("id = %q", body.ID)
	}
}

func TestModelDetailUnknownModel(t *testing.T) {
	cfg := &config.Config{}
	raw, hash, prefix, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	req := httptest.NewRequest("GET", "/v1/models/nope", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "invalid_request_error" || body.Error.Code != "model_not_found" {
		t.Fatalf("error = %+v", body.Error)
	}
}

type modelDetailResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func requestModelDetail(t *testing.T, cfg *config.Config, path string) modelDetailResponse {
	t.Helper()
	raw, hash, prefix, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	v := &vault.Vault{ClientKeys: []vault.ClientKey{{ID: "k1", KeyHash: hash, Prefix: prefix, Active: true}}}
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	testRouter(t, cfg, v).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body modelDetailResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}
