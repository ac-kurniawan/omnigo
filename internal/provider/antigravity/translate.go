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
	messages := requestMessages(req)
	system, contents, err := geminiContents(messages)
	if err != nil {
		return nil, err
	}
	inner := map[string]any{"contents": contents}
	if system != nil {
		inner["systemInstruction"] = system
	}
	if config := generationConfig(req.Parsed); config != nil {
		inner["generationConfig"] = config
	}
	decls, err := functionDeclarations(req.Parsed)
	if err != nil {
		return nil, err
	}
	if len(decls) > 0 {
		inner["tools"] = []any{map[string]any{"functionDeclarations": decls}}
		if mode, allowed := toolChoice(req.Parsed); mode != "" {
			calling := map[string]any{"mode": mode}
			if allowed != "" {
				calling["allowedFunctionNames"] = []string{allowed}
			}
			inner["toolConfig"] = map[string]any{"functionCallingConfig": calling}
		}
	}
	return map[string]any{
		"project":   projectID,
		"requestId": newRequestID(),
		"request":   inner,
		"model":     model,
		"userAgent": "antigravity/ide/0.0.0 darwin/arm64",
	}, nil
}

// requestMessages prefers the parsed chat body. The typed Messages field drops
// tool calls, tool results, and the tool_call_id a result has to answer.
func requestMessages(req provider.ChatRequest) []any {
	if req.Parsed != nil {
		if messages, ok := req.Parsed["messages"].([]any); ok {
			return messages
		}
	}
	out := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		out = append(out, map[string]any{"role": m.Role, "content": m.Content})
	}
	return out
}

func geminiContents(messages []any) (map[string]any, []any, error) {
	var systemParts []any
	contents := make([]any, 0, len(messages))
	started := false
	for i := 0; i < len(messages); i++ {
		message, ok := messages[i].(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("antigravity: messages[%d] must be an object", i)
		}
		role, _ := message["role"].(string)
		if (role == "system" || role == "developer") && !started {
			systemParts = append(systemParts, textParts(message["content"])...)
			continue
		}
		started = true
		switch role {
		case "system", "developer", "user":
			parts := textParts(message["content"])
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, map[string]any{"role": "user", "parts": parts})
		case "assistant":
			parts := textParts(message["content"])
			calls, err := assistantCalls(message, i)
			if err != nil {
				return nil, nil, err
			}
			parts = append(parts, calls...)
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, map[string]any{"role": "model", "parts": parts})
			if len(calls) == 0 {
				continue
			}
			replies, consumed, err := toolReplies(messages[i+1:], calls)
			if err != nil {
				return nil, nil, err
			}
			if len(replies) > 0 {
				contents = append(contents, map[string]any{"role": "user", "parts": replies})
			}
			i += consumed
		case "tool":
			continue
		default:
			parts := textParts(message["content"])
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, map[string]any{"role": "user", "parts": parts})
		}
	}
	if systemParts == nil {
		return nil, contents, nil
	}
	return map[string]any{"role": "user", "parts": systemParts}, contents, nil
}

func textParts(content any) []any {
	switch content := content.(type) {
	case string:
		if content == "" {
			return nil
		}
		return []any{map[string]any{"text": content}}
	case []any:
		parts := make([]any, 0, len(content))
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok || part["type"] != "text" {
				continue
			}
			text, _ := part["text"].(string)
			if text == "" {
				continue
			}
			parts = append(parts, map[string]any{"text": text})
		}
		return parts
	default:
		return nil
	}
}

// thoughtSignatureSentinel is the value Gemini 3 accepts on a replayed function
// call whose original signature the client did not keep.
const thoughtSignatureSentinel = "skip_thought_signature_validator"

func assistantCalls(message map[string]any, index int) ([]any, error) {
	raw, ok := message["tool_calls"]
	if !ok || raw == nil {
		return nil, nil
	}
	calls, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("antigravity: messages[%d].tool_calls must be an array", index)
	}
	parts := make([]any, 0, len(calls))
	for i, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("antigravity: messages[%d].tool_calls[%d] must be an object", index, i)
		}
		if kind, _ := call["type"].(string); kind != "" && kind != "function" {
			continue
		}
		function, _ := call["function"].(map[string]any)
		name, _ := function["name"].(string)
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name, "args": callArgs(function["arguments"])}
		if id, _ := call["id"].(string); id != "" {
			fn["id"] = id
		}
		parts = append(parts, map[string]any{
			"functionCall":     fn,
			"thoughtSignature": thoughtSignatureSentinel,
		})
	}
	return parts, nil
}

func callArgs(raw any) any {
	text, _ := raw.(string)
	if text == "" {
		return map[string]any{}
	}
	var args any
	if err := json.Unmarshal([]byte(text), &args); err != nil {
		return map[string]any{"params": text}
	}
	if args == nil {
		return map[string]any{}
	}
	return args
}

func toolReplies(rest []any, calls []any) ([]any, int, error) {
	byID := map[string]map[string]any{}
	consumed := 0
	for _, raw := range rest {
		message, ok := raw.(map[string]any)
		if !ok {
			break
		}
		role, _ := message["role"].(string)
		if role == "assistant" {
			break
		}
		consumed++
		if role != "tool" {
			continue
		}
		id, _ := message["tool_call_id"].(string)
		if id == "" {
			continue
		}
		byID[id] = message
	}
	parts := make([]any, 0, len(calls))
	for _, raw := range calls {
		call := raw.(map[string]any)["functionCall"].(map[string]any)
		id, _ := call["id"].(string)
		name, _ := call["name"].(string)
		result := "{}"
		if reply, ok := byID[id]; ok {
			if text := contentText(reply["content"]); text != "" {
				result = text
			}
		}
		fn := map[string]any{"name": name, "response": map[string]any{"result": result}}
		if id != "" {
			fn["id"] = id
		}
		parts = append(parts, map[string]any{"functionResponse": fn})
	}
	return parts, consumed, nil
}

func contentText(content any) string {
	switch content := content.(type) {
	case string:
		return content
	case []any:
		var b strings.Builder
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok || part["type"] != "text" {
				continue
			}
			text, _ := part["text"].(string)
			b.WriteString(text)
		}
		return b.String()
	default:
		return ""
	}
}

func generationConfig(parsed map[string]any) map[string]any {
	if parsed == nil {
		return nil
	}
	config := map[string]any{}
	if n, ok := numberValue(parsed["max_tokens"]); ok {
		config["maxOutputTokens"] = n
	} else if n, ok := numberValue(parsed["max_completion_tokens"]); ok {
		config["maxOutputTokens"] = n
	}
	if n, ok := numberValue(parsed["temperature"]); ok {
		config["temperature"] = n
	}
	if n, ok := numberValue(parsed["top_p"]); ok {
		config["topP"] = n
	}
	if len(config) == 0 {
		return nil
	}
	return config
}

func numberValue(raw any) (float64, bool) {
	switch n := raw.(type) {
	case json.Number:
		v, err := n.Float64()
		return v, err == nil
	case float64:
		return n, true
	default:
		return 0, false
	}
}

func functionDeclarations(parsed map[string]any) ([]any, error) {
	if parsed == nil {
		return nil, nil
	}
	raw, ok := parsed["tools"]
	if !ok || raw == nil {
		return nil, nil
	}
	tools, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("antigravity: tools must be an array")
	}
	decls := make([]any, 0, len(tools))
	seen := map[string]struct{}{}
	for i, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("antigravity: tools[%d] must be an object", i)
		}
		if kind, _ := tool["type"].(string); kind != "" && kind != "function" {
			continue
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := function["name"].(string)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		decl := map[string]any{"name": name}
		if description, _ := function["description"].(string); description != "" {
			decl["description"] = description
		}
		decl["parameters"] = declarationSchema(function["parameters"])
		decls = append(decls, decl)
	}
	return decls, nil
}

func declarationSchema(raw any) map[string]any {
	schema, ok := raw.(map[string]any)
	if !ok || len(schema) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	cleaned := cleanSchema(cloneMap(schema)).(map[string]any)
	if _, ok := cleaned["type"]; !ok {
		if _, hasProps := cleaned["properties"]; hasProps {
			cleaned["type"] = "object"
		}
	}
	return cleaned
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = cloneValue(value)
	}
	return out
}

func cloneValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneMap(value)
	case []any:
		out := make([]any, len(value))
		for i := range value {
			out[i] = cloneValue(value[i])
		}
		return out
	default:
		return value
	}
}

// unsupportedSchemaKeys are JSON Schema keywords Gemini function declarations
// reject. Dropping them keeps the declaration the model can still call.
var unsupportedSchemaKeys = map[string]struct{}{
	"additionalProperties":  {},
	"$schema":               {},
	"$id":                   {},
	"$ref":                  {},
	"$defs":                 {},
	"definitions":           {},
	"strict":                {},
	"const":                 {},
	"pattern":               {},
	"format":                {},
	"minLength":             {},
	"maxLength":             {},
	"minimum":               {},
	"maximum":               {},
	"exclusiveMinimum":      {},
	"exclusiveMaximum":      {},
	"minItems":              {},
	"maxItems":              {},
	"uniqueItems":           {},
	"patternProperties":     {},
	"unevaluatedProperties": {},
	"propertyNames":         {},
	"contentMediaType":      {},
	"contentEncoding":       {},
}

func cleanSchema(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key := range unsupportedSchemaKeys {
			delete(value, key)
		}
		// Gemini's Schema.type is a single enum; JSON Schema unions like
		// ["number","null"] are rejected with "Proto field is not repeating".
		// Keep the first non-null type and express null via nullable.
		if types, ok := value["type"].([]any); ok {
			delete(value, "type")
			for _, t := range types {
				name, _ := t.(string)
				if name == "null" {
					value["nullable"] = true
				} else if _, set := value["type"]; !set && name != "" {
					value["type"] = name
				}
			}
		}
		for key, child := range value {
			if key == "properties" {
				if properties, ok := child.(map[string]any); ok {
					for name, schema := range properties {
						properties[name] = cleanSchema(schema)
					}
					continue
				}
			}
			value[key] = cleanSchema(child)
		}
		if required, ok := value["required"].([]any); ok {
			props, _ := value["properties"].(map[string]any)
			kept := required[:0]
			for _, name := range required {
				text, ok := name.(string)
				if !ok {
					continue
				}
				if _, exists := props[text]; exists {
					kept = append(kept, text)
				}
			}
			if len(kept) == 0 {
				delete(value, "required")
			} else {
				value["required"] = kept
			}
		}
		return value
	case []any:
		for i := range value {
			value[i] = cleanSchema(value[i])
		}
		return value
	default:
		return value
	}
}

func toolChoice(parsed map[string]any) (string, string) {
	if parsed == nil {
		return "", ""
	}
	raw, ok := parsed["tool_choice"]
	if !ok || raw == nil {
		return "", ""
	}
	switch choice := raw.(type) {
	case string:
		switch strings.ToLower(choice) {
		case "none":
			return "NONE", ""
		case "auto":
			return "AUTO", ""
		case "required", "any":
			return "ANY", ""
		default:
			return "", ""
		}
	case map[string]any:
		kind, _ := choice["type"].(string)
		if strings.ToLower(kind) != "function" {
			return "", ""
		}
		function, _ := choice["function"].(map[string]any)
		name, _ := function["name"].(string)
		return "ANY", name
	default:
		return "", ""
	}
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

// parseGeminiFrame decodes one SSE data field. The spec allows the value to
// follow the colon with or without a space, so both shapes decode. Frames are
// decoded once: the stream loop needs the finish reason, the content, and the
// usage of the same frame.
func parseGeminiFrame(chunk []byte) (geminiBody, error) {
	payload := string(chunk)
	if rest, ok := strings.CutPrefix(payload, "data:"); ok {
		payload = rest
	}
	payload = strings.TrimSpace(payload)
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

// tokenUsage is the accounting a frame reported. Present is false when the
// frame carried no usageMetadata, which is not the same as zero tokens.
func (b geminiBody) tokenUsage() provider.TokenUsage {
	if b.UsageMetadata == nil {
		return provider.TokenUsage{}
	}
	return provider.TokenUsage{
		InputTokens:  b.UsageMetadata.PromptTokens,
		OutputTokens: b.UsageMetadata.CompletionTokens,
		Present:      true,
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
	case reason == "SAFETY":
		return "content_filter"
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
