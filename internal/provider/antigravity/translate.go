package antigravity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func role(r string) string {
	switch r {
	case "assistant":
		return "model"
	default:
		return "user"
	}
}

func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "req-fallback"
	}
	return "req-" + hex.EncodeToString(b)
}

func ToEnvelope(projectID, model string, req provider.ChatRequest) (map[string]any, error) {
	contents := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		parts := make([]any, 0, 1)
		switch content := m.Content.(type) {
		case string:
			parts = append(parts, map[string]any{"text": content})
		case []any:
			for _, rawPart := range content {
				part, ok := rawPart.(map[string]any)
				if !ok || part["type"] != "text" {
					continue
				}
				if text, ok := part["text"].(string); ok {
					parts = append(parts, map[string]any{"text": text})
				}
			}
		}
		contents = append(contents, map[string]any{
			"role":  role(m.Role),
			"parts": parts,
		})
	}
	return map[string]any{
		"project":     projectID,
		"requestId":   newRequestID(),
		"request":     map[string]any{"contents": contents},
		"model":       model,
		"userAgent":   "antigravity/ide/0.0.0 darwin/arm64",
		"requestType": "agent",
	}, nil
}

type geminiChunk struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

func geminiChunkText(chunk []byte) (string, error) {
	payload := strings.TrimSpace(strings.TrimPrefix(string(chunk), "data: "))
	var body geminiChunk
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		return "", fmt.Errorf("parse gemini chunk: %w", err)
	}
	if len(body.Candidates) == 0 {
		return "", nil
	}
	var text strings.Builder
	for _, part := range body.Candidates[0].Content.Parts {
		text.WriteString(part.Text)
	}
	return text.String(), nil
}

// TranslateSSE converts one Gemini SSE `data:` line into an OpenAI SSE
// `data:` line, or returns nil when the chunk carries no candidate text.
func TranslateSSE(geminiChunk []byte) ([]byte, error) {
	text, err := geminiChunkText(geminiChunk)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, nil
	}
	out := map[string]any{
		"id":      "chatcmpl-omnigo",
		"object":  "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}},
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return append([]byte("data: "), append(b, '\n', '\n')...), nil
}
