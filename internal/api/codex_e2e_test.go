package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestCodexChatCompletionsEndToEnd(t *testing.T) {
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			upstreamCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer access-test" || r.Header.Get("chatgpt-account-id") != "account-test" {
				t.Error("unexpected upstream authentication headers")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, codexSSE("response.created", map[string]any{"response": map[string]any{"id": "resp_e2e", "model": "gpt-test"}})+codexSSE("response.output_text.delta", map[string]any{"delta": "hello"})+codexSSE("response.completed", map[string]any{"response": map[string]any{"id": "resp_e2e", "model": "gpt-test", "status": "completed", "usage": map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3}}}))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := &config.Config{Providers: []config.Provider{{Name: "codex-main", Type: "codex", BaseURL: server.URL + "/responses", Models: []string{"gpt-test"}}}}
	rawKey, hash, prefix, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	vaultData := &vault.Vault{
		ClientKeys:      []vault.ClientKey{{ID: "e2e", KeyHash: hash, Prefix: prefix, Active: true}},
		ProviderSecrets: map[string]vault.ProviderSecret{"codex-main": {AccessToken: "access-test", RefreshToken: "refresh-test", AccountID: "account-test", ExpiresAt: time.Now().Add(time.Hour)}},
	}
	router := testRouter(t, cfg, vaultData)

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsReq.Header.Set("Authorization", "Bearer "+rawKey)
	modelsRR := httptest.NewRecorder()
	router.ServeHTTP(modelsRR, modelsReq)
	if modelsRR.Code != http.StatusOK || !strings.Contains(modelsRR.Body.String(), `"id":"codex-main/gpt-test"`) {
		t.Fatalf("models status=%d body=%s", modelsRR.Code, modelsRR.Body.String())
	}

	for _, stream := range []bool{false, true} {
		body := `{"model":"codex-main/gpt-test","messages":[{"role":"user","content":"hi"}],"stream":` + strconv.FormatBool(stream) + `}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+rawKey)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("stream=%v status=%d body=%s", stream, rr.Code, rr.Body.String())
		}
		if stream {
			if !strings.Contains(rr.Body.String(), `"content":"hello"`) || !strings.HasSuffix(rr.Body.String(), "data: [DONE]\n\n") {
				t.Fatalf("stream response = %s", rr.Body.String())
			}
		} else if !strings.Contains(rr.Body.String(), `"object":"chat.completion"`) || !strings.Contains(rr.Body.String(), `"content":"hello"`) || !strings.Contains(rr.Body.String(), `"total_tokens":3`) {
			t.Fatalf("response = %s", rr.Body.String())
		}
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("upstream calls=%d", upstreamCalls.Load())
	}
}

func codexSSE(typ string, fields map[string]any) string {
	fields["type"] = typ
	body, _ := json.Marshal(fields)
	return "event: " + typ + "\ndata: " + string(body) + "\n\n"
}
