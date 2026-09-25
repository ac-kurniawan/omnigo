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
		if len(parts) == 0 {
			continue
		}
		contents = append(contents, map[string]any{
			"role":  role(m.Role),
			"parts": parts,
		})
	}
	return map[string]any{
		"project":   projectID,
		"requestId": newRequestID(),
		"request":   map[string]any{"contents": contents},
		"model":     model,
		"userAgent": "antigravity/ide/0.0.0 darwin/arm64",
	}, nil
}

// geminiCall is a function call the model asked for. Args stays raw so the
// arguments reach the client byte for byte.
type geminiCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// geminiPart is one part of a candidate's content: visible text, thinking
// text, or a function call. A thinking part carries its text plus the flag.
type geminiPart struct {
	Text         string      `json:"text"`
	Thought      bool        `json:"thought"`
	FunctionCall *geminiCall `json:"functionCall"`
}

type geminiCandidate struct {
	Content struct {
		Parts []geminiPart `json:"parts"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
}

// geminiUsage is the token accounting. The upstream reports it on the frame
// that ends the generation.
type geminiUsage struct {
	PromptTokens     int `json:"promptTokenCount"`
	CompletionTokens int `json:"candidatesTokenCount"`
	TotalTokens      int `json:"totalTokenCount"`
}

// geminiBody is the payload of one SSE frame.
type geminiBody struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsage      `json:"usageMetadata"`
}

// geminiFrame is one SSE frame. The daily-cloudcode-pa host wraps the payload
// in "response"; the legacy host sends it bare. Both decode into geminiBody.
type geminiFrame struct {
	Response *geminiBody `json:"response"`
	geminiBody
}

// parseGeminiFrame decodes one SSE `data:` line. Frames are decoded once: the
// stream loop needs the finish reason, the content, and the usage of the same
// frame, and decoding it per question cost three passes.
func parseGeminiFrame(chunk []byte) (geminiBody, error) {
	payload := strings.TrimSpace(strings.TrimPrefix(string(chunk), "data: "))
	var frame geminiFrame
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		return geminiBody{}, fmt.Errorf("parse gemini chunk: %w", err)
	}
	if frame.Response != nil {
		return *frame.Response, nil
	}
	return frame.geminiBody, nil
}

// candidate returns the first candidate, or false for a frame that carries
// none: a metadata-only frame, or the empty candidate list some hosts send.
func (b geminiBody) candidate() (geminiCandidate, bool) {
	if len(b.Candidates) == 0 {
		return geminiCandidate{}, false
	}
	return b.Candidates[0], true
}

// finishReason reports why the generation ended, or "" while it is still
// running. Most frames carry none; the last one does.
func (b geminiBody) finishReason() string {
	candidate, ok := b.candidate()
	if !ok {
		return ""
	}
	return candidate.FinishReason
}

// usageMap renders the token accounting as an OpenAI usage object, or nil when
// the frame reported none.
func (b geminiBody) usageMap() map[string]any {
	if b.UsageMetadata == nil {
		return nil
	}
	return map[string]any{
		"prompt_tokens":     b.UsageMetadata.PromptTokens,
		"completion_tokens": b.UsageMetadata.CompletionTokens,
		"total_tokens":      b.UsageMetadata.TotalTokens,
	}
}

// content splits the candidate's parts into visible text, thinking text, and
// function calls. next indexes the calls already seen in this generation, so a
// call keeps the same id when a later frame repeats it.
func (b geminiBody) content(next int) (string, string, []any) {
	candidate, ok := b.candidate()
	if !ok {
		return "", "", nil
	}
	var text, thought strings.Builder
	var calls []any
	for _, part := range candidate.Content.Parts {
		switch {
		case part.FunctionCall != nil:
			index := next + len(calls)
			calls = append(calls, map[string]any{
				"index": index, "id": fmt.Sprintf("call_%d", index), "type": "function",
				"function": map[string]any{
					"name":      part.FunctionCall.Name,
					"arguments": callArguments(part.FunctionCall.Args),
				},
			})
		case part.Thought:
			thought.WriteString(part.Text)
		default:
			text.WriteString(part.Text)
		}
	}
	return text.String(), thought.String(), calls
}

// callArguments renders function-call arguments as the JSON string OpenAI
// clients parse. A call without arguments becomes an empty object.
func callArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// finishReason maps a Gemini finish reason onto the OpenAI vocabulary. A
// candidate that asked for a function call always finishes as tool_calls: the
// client has to run the call before the turn can continue.
func finishReason(reason string, calls int) string {
	switch {
	case calls > 0:
		return "tool_calls"
	case reason == "MAX_TOKENS":
		return "length"
	default:
		return "stop"
	}
}

// sseChunk renders one OpenAI streaming chunk as an SSE record. usage rides on
// the terminal chunk, the shape OpenAI clients read token counts from.
func sseChunk(delta map[string]any, reason any, usage map[string]any) ([]byte, error) {
	chunk := map[string]any{
		"id":      "chatcmpl-omnigo",
		"object":  "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return append([]byte("data: "), append(encoded, '\n', '\n')...), nil
}

// TranslateSSE converts one Gemini SSE `data:` line into the OpenAI SSE lines
// it maps to, or returns nil for a frame with nothing to forward.
func TranslateSSE(geminiChunk []byte) ([]byte, error) {
	body, err := parseGeminiFrame(geminiChunk)
	if err != nil {
		return nil, err
	}
	out, _, _, err := frameToSSE(body, 0)
	return out, err
}

// frameToSSE renders one frame as OpenAI SSE records, one chunk per kind of
// content it carries. next is the index of the first function call in this
// frame. A finish reason lands on the last chunk, together with the usage. A
// frame that carries only the finish reason becomes a chunk with an empty
// delta. A frame with neither content nor a finish reason maps to nothing. The
// bool reports whether the frame ended the generation.
func frameToSSE(body geminiBody, next int) ([]byte, bool, int, error) {
	usage := body.usageMap()
	text, thought, calls := body.content(next)
	var deltas []map[string]any
	if text != "" {
		deltas = append(deltas, map[string]any{"content": text})
	}
	if thought != "" {
		deltas = append(deltas, map[string]any{"reasoning_content": thought})
	}
	if len(calls) > 0 {
		deltas = append(deltas, map[string]any{"tool_calls": calls})
	}
	reason := body.finishReason()
	finished := reason != ""
	var out []byte
	for i, delta := range deltas {
		var mapped any
		var chunkUsage map[string]any
		if finished && i == len(deltas)-1 {
			mapped = finishReason(reason, len(calls))
			chunkUsage = usage
		}
		record, err := sseChunk(delta, mapped, chunkUsage)
		if err != nil {
			return nil, false, 0, err
		}
		out = append(out, record...)
	}
	if finished && len(deltas) == 0 {
		record, err := sseChunk(map[string]any{}, finishReason(reason, 0), usage)
		return record, true, len(calls), err
	}
	return out, finished, len(calls), nil
}
