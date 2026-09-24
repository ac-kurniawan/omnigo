package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

var errInvalidTranslation = errors.New("invalid Codex translation")

func ToResponsesRequest(req provider.ChatRequest) (map[string]any, error) {
	var root map[string]any
	switch {
	case req.Parsed != nil:
		root = requestRoot(req.Parsed)
	case len(req.Raw) == 0:
		messages := make([]any, 0, len(req.Messages))
		for _, message := range req.Messages {
			messages = append(messages, map[string]any{"role": message.Role, "content": message.Content})
		}
		root = map[string]any{"messages": messages}
	default:
		dec := json.NewDecoder(bytes.NewReader(req.Raw))
		dec.UseNumber()
		if err := dec.Decode(&root); err != nil {
			return nil, fmt.Errorf("decode Codex request: %w: %v", errInvalidTranslation, err)
		}
		if root == nil {
			return nil, fmt.Errorf("decode Codex request root: %w", errInvalidTranslation)
		}
		if err := ensureEOF(dec); err != nil {
			return nil, fmt.Errorf("decode Codex request: %w", err)
		}
	}

	messages, err := requiredSlice(root, "messages")
	if err != nil {
		return nil, err
	}
	instructions := make([]string, 0)
	input := make([]any, 0, len(messages))
	for i, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			return nil, invalid("messages[%d] must be an object", i)
		}
		role, err := requiredString(message, "role", fmt.Sprintf("messages[%d]", i))
		if err != nil {
			return nil, err
		}
		switch role {
		case "system", "developer":
			parts, err := instructionParts(message["content"], fmt.Sprintf("messages[%d].content", i))
			if err != nil {
				return nil, err
			}
			instructions = append(instructions, parts...)
		case "user", "assistant":
			if content, exists := message["content"]; exists && content != nil {
				parts, err := messageParts(content, role, fmt.Sprintf("messages[%d].content", i))
				if err != nil {
					return nil, err
				}
				if len(parts) > 0 {
					input = append(input, map[string]any{"type": "message", "role": role, "content": parts})
				}
			}
			if role == "assistant" {
				calls, err := toolCalls(message, i)
				if err != nil {
					return nil, err
				}
				input = append(input, calls...)
			}
		case "tool":
			output, err := toolOutput(message, i)
			if err != nil {
				return nil, err
			}
			input = append(input, output)
		default:
			return nil, invalid("messages[%d].role %q is unsupported", i, role)
		}
	}

	out := map[string]any{
		"model":   req.Model,
		"input":   input,
		"store":   false,
		"stream":  true,
		"include": []any{"reasoning.encrypted_content"},
	}
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n")
	}
	if raw, ok := root["tools"]; ok {
		tools, err := translateTools(raw)
		if err != nil {
			return nil, err
		}
		out["tools"] = tools
	}
	if raw, ok := root["tool_choice"]; ok {
		choice, err := translateToolChoice(raw)
		if err != nil {
			return nil, err
		}
		out["tool_choice"] = choice
	}
	if raw, ok := root["parallel_tool_calls"]; ok {
		value, ok := raw.(bool)
		if !ok {
			return nil, invalid("parallel_tool_calls must be a boolean")
		}
		out["parallel_tool_calls"] = value
	}
	if raw, ok := root["reasoning_effort"]; ok {
		effort, ok := raw.(string)
		if !ok {
			return nil, invalid("reasoning_effort must be a string")
		}
		out["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	} else if raw, ok := root["reasoning"]; ok {
		reasoning, err := translateReasoning(raw)
		if err != nil {
			return nil, err
		}
		out["reasoning"] = reasoning
	}
	if raw, ok := root["response_format"]; ok {
		text, err := translateResponseFormat(raw)
		if err != nil {
			return nil, err
		}
		out["text"] = map[string]any{"format": text}
		if text["type"] == "json_object" {
			ensureJSONMention(input)
		}
	}
	return out, nil
}

func requestRoot(parsed map[string]any) map[string]any {
	root := make(map[string]any, len(parsed))
	for key, value := range parsed {
		if key == provider.OpenAIBodyCacheKey() {
			continue
		}
		root[key] = value
	}
	return root
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return invalid("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func requiredSlice(object map[string]any, key string) ([]any, error) {
	raw, ok := object[key]
	if !ok {
		return nil, invalid("%s is required", key)
	}
	value, ok := raw.([]any)
	if !ok {
		return nil, invalid("%s must be an array", key)
	}
	return value, nil
}

func requiredString(object map[string]any, key, path string) (string, error) {
	raw, ok := object[key]
	if !ok {
		return "", invalid("%s.%s is required", path, key)
	}
	value, ok := raw.(string)
	if !ok {
		return "", invalid("%s.%s must be a string", path, key)
	}
	return value, nil
}

func optionalString(object map[string]any, key, path string) (string, bool, error) {
	raw, ok := object[key]
	if !ok {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", false, invalid("%s.%s must be a string", path, key)
	}
	return value, true, nil
}

func optionalBool(object map[string]any, key, path string) (bool, bool, error) {
	raw, ok := object[key]
	if !ok {
		return false, false, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return false, false, invalid("%s.%s must be a boolean", path, key)
	}
	return value, true, nil
}

func instructionParts(raw any, path string) ([]string, error) {
	if text, ok := raw.(string); ok {
		return []string{text}, nil
	}
	parts, ok := raw.([]any)
	if !ok {
		return nil, invalid("%s must be a string or array", path)
	}
	out := make([]string, 0, len(parts))
	for i, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return nil, invalid("%s[%d] must be an object", path, i)
		}
		typeName, err := requiredString(part, "type", fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		if typeName != "text" {
			return nil, invalid("%s[%d].type %q is unsupported", path, i, typeName)
		}
		text, err := requiredString(part, "text", fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		out = append(out, text)
	}
	return out, nil
}

func messageParts(raw any, role, path string) ([]any, error) {
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}
	if text, ok := raw.(string); ok {
		return []any{map[string]any{"type": textType, "text": text}}, nil
	}
	parts, ok := raw.([]any)
	if !ok {
		return nil, invalid("%s must be a string or array", path)
	}
	out := make([]any, 0, len(parts))
	for i, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return nil, invalid("%s[%d] must be an object", path, i)
		}
		typeName, err := requiredString(part, "type", fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		switch typeName {
		case "text":
			text, err := requiredString(part, "text", fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"type": textType, "text": text})
		case "image_url":
			if role != "user" {
				return nil, invalid("%s[%d] image is unsupported for assistant", path, i)
			}
			image, ok := part["image_url"].(map[string]any)
			if !ok {
				return nil, invalid("%s[%d].image_url must be an object", path, i)
			}
			url, err := requiredString(image, "url", fmt.Sprintf("%s[%d].image_url", path, i))
			if err != nil {
				return nil, err
			}
			translated := map[string]any{"type": "input_image", "image_url": url}
			if detail, exists, err := optionalString(image, "detail", fmt.Sprintf("%s[%d].image_url", path, i)); err != nil {
				return nil, err
			} else if exists {
				translated["detail"] = detail
			}
			out = append(out, translated)
		default:
			return nil, invalid("%s[%d].type %q is unsupported", path, i, typeName)
		}
	}
	return out, nil
}

func toolCalls(message map[string]any, messageIndex int) ([]any, error) {
	raw, ok := message["tool_calls"]
	if !ok {
		return nil, nil
	}
	calls, ok := raw.([]any)
	if !ok {
		return nil, invalid("messages[%d].tool_calls must be an array", messageIndex)
	}
	out := make([]any, 0, len(calls))
	for i, rawCall := range calls {
		path := fmt.Sprintf("messages[%d].tool_calls[%d]", messageIndex, i)
		call, ok := rawCall.(map[string]any)
		if !ok {
			return nil, invalid("%s must be an object", path)
		}
		typeName, err := requiredString(call, "type", path)
		if err != nil {
			return nil, err
		}
		if typeName != "function" {
			return nil, invalid("%s.type %q is unsupported", path, typeName)
		}
		callID, err := requiredString(call, "id", path)
		if err != nil {
			return nil, err
		}
		function, ok := call["function"].(map[string]any)
		if !ok {
			return nil, invalid("%s.function must be an object", path)
		}
		name, err := requiredString(function, "name", path+".function")
		if err != nil {
			return nil, err
		}
		arguments, err := requiredString(function, "arguments", path+".function")
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"type": "function_call", "call_id": callID, "name": name, "arguments": arguments})
	}
	return out, nil
}

func toolOutput(message map[string]any, messageIndex int) (map[string]any, error) {
	path := fmt.Sprintf("messages[%d]", messageIndex)
	callID, err := requiredString(message, "tool_call_id", path)
	if err != nil {
		return nil, err
	}
	output, err := requiredString(message, "content", path)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "function_call_output", "call_id": callID, "output": output}, nil
}

func translateTools(raw any) ([]any, error) {
	tools, ok := raw.([]any)
	if !ok {
		return nil, invalid("tools must be an array")
	}
	out := make([]any, 0, len(tools))
	for i, rawTool := range tools {
		path := fmt.Sprintf("tools[%d]", i)
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return nil, invalid("%s must be an object", path)
		}
		typeName, err := requiredString(tool, "type", path)
		if err != nil {
			return nil, err
		}
		if typeName != "function" {
			return nil, invalid("%s.type %q is unsupported", path, typeName)
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			return nil, invalid("%s.function must be an object", path)
		}
		name, err := requiredString(function, "name", path+".function")
		if err != nil {
			return nil, err
		}
		translated := map[string]any{"type": "function", "name": name}
		if description, exists, err := optionalString(function, "description", path+".function"); err != nil {
			return nil, err
		} else if exists {
			translated["description"] = description
		}
		if parameters, exists := function["parameters"]; exists {
			if _, ok := parameters.(map[string]any); !ok {
				return nil, invalid("%s.function.parameters must be an object", path)
			}
			translated["parameters"] = parameters
		}
		if strict, exists, err := optionalBool(function, "strict", path+".function"); err != nil {
			return nil, err
		} else if exists {
			translated["strict"] = strict
		}
		out = append(out, translated)
	}
	return out, nil
}

func translateToolChoice(raw any) (any, error) {
	if choice, ok := raw.(string); ok {
		switch choice {
		case "none", "auto", "required":
			return choice, nil
		default:
			return nil, invalid("tool_choice %q is unsupported", choice)
		}
	}
	choice, ok := raw.(map[string]any)
	if !ok {
		return nil, invalid("tool_choice must be a string or object")
	}
	typeName, err := requiredString(choice, "type", "tool_choice")
	if err != nil {
		return nil, err
	}
	if typeName != "function" {
		return nil, invalid("tool_choice.type %q is unsupported", typeName)
	}
	function, ok := choice["function"].(map[string]any)
	if !ok {
		return nil, invalid("tool_choice.function must be an object")
	}
	name, err := requiredString(function, "name", "tool_choice.function")
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "function", "name": name}, nil
}

func translateReasoning(raw any) (map[string]any, error) {
	reasoning, ok := raw.(map[string]any)
	if !ok {
		return nil, invalid("reasoning must be an object")
	}
	out := map[string]any{}
	if effort, exists, err := optionalString(reasoning, "effort", "reasoning"); err != nil {
		return nil, err
	} else if exists {
		out["effort"] = effort
	}
	if summary, exists, err := optionalString(reasoning, "summary", "reasoning"); err != nil {
		return nil, err
	} else if exists {
		out["summary"] = summary
	}
	if len(out) == 0 {
		return nil, invalid("reasoning has no supported fields")
	}
	return out, nil
}

func translateResponseFormat(raw any) (map[string]any, error) {
	format, ok := raw.(map[string]any)
	if !ok {
		return nil, invalid("response_format must be an object")
	}
	typeName, err := requiredString(format, "type", "response_format")
	if err != nil {
		return nil, err
	}
	switch typeName {
	case "text", "json_object":
		return map[string]any{"type": typeName}, nil
	case "json_schema":
		schema, ok := format["json_schema"].(map[string]any)
		if !ok {
			return nil, invalid("response_format.json_schema must be an object")
		}
		name, err := requiredString(schema, "name", "response_format.json_schema")
		if err != nil {
			return nil, err
		}
		schemaValue, ok := schema["schema"].(map[string]any)
		if !ok {
			return nil, invalid("response_format.json_schema.schema must be an object")
		}
		out := map[string]any{"type": "json_schema", "name": name, "schema": schemaValue}
		if description, exists, err := optionalString(schema, "description", "response_format.json_schema"); err != nil {
			return nil, err
		} else if exists {
			out["description"] = description
		}
		if strict, exists, err := optionalBool(schema, "strict", "response_format.json_schema"); err != nil {
			return nil, err
		} else if exists {
			out["strict"] = strict
		}
		return out, nil
	default:
		return nil, invalid("response_format.type %q is unsupported", typeName)
	}
}

// jsonObjectHint is appended when a json_object response is requested but no
// user message contains the word "json". Codex enforces that itself and rejects
// the request with a 400 otherwise, even when a system message already says it.
const jsonObjectHint = "Respond in json."

// ensureJSONMention makes a json_object request acceptable to Codex, which
// requires the word "json" somewhere in the user input and only inspects the
// last user message. It appends a short hint to that message when the word is
// absent, and does nothing when the caller already satisfied the requirement.
func ensureJSONMention(input []any) {
	for i := len(input) - 1; i >= 0; i-- {
		message, ok := input[i].(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		parts, ok := message["content"].([]any)
		if !ok {
			return
		}
		for _, part := range parts {
			item, ok := part.(map[string]any)
			if !ok {
				continue
			}
			text, _ := item["text"].(string)
			if strings.Contains(strings.ToLower(text), "json") {
				return
			}
		}
		message["content"] = append(parts, map[string]any{"type": "input_text", "text": jsonObjectHint})
		return
	}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInvalidTranslation, fmt.Sprintf(format, args...))
}

type responseEvent struct {
	Type        string
	Delta       string
	OutputIndex int
	ItemID      string
	Item        responseItem
	Response    responseEnvelope
}

type responseItem struct {
	ID        string
	Type      string
	CallID    string
	Name      string
	Arguments string
}

type responseEnvelope struct {
	ID               string
	Model            string
	Status           string
	Usage            *responseUsage
	Error            bool
	IncompleteReason string
}

type responseUsage struct {
	InputTokens     int
	OutputTokens    int
	TotalTokens     int
	ReasoningTokens int
}

func parseResponseEvent(payload []byte) (responseEvent, error) {
	payload = bytes.TrimSpace(payload)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	var raw struct {
		Type        any `json:"type"`
		Delta       any `json:"delta"`
		OutputIndex any `json:"output_index"`
		ItemID      any `json:"item_id"`
		Item        any `json:"item"`
		Response    any `json:"response"`
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return responseEvent{}, fmt.Errorf("parse Codex event: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return responseEvent{}, fmt.Errorf("parse Codex event: %w", err)
	}
	typeName, ok := raw.Type.(string)
	if !ok || typeName == "" {
		return responseEvent{}, invalid("event type must be a non-empty string")
	}
	event := responseEvent{Type: typeName}
	if raw.Delta != nil {
		delta, ok := raw.Delta.(string)
		if !ok {
			return responseEvent{}, invalid("event delta must be a string")
		}
		event.Delta = delta
	}
	if raw.OutputIndex != nil {
		index, err := jsonInt(raw.OutputIndex, "event output_index")
		if err != nil {
			return responseEvent{}, err
		}
		event.OutputIndex = index
	}
	if raw.ItemID != nil {
		itemID, ok := raw.ItemID.(string)
		if !ok {
			return responseEvent{}, invalid("event item_id must be a string")
		}
		event.ItemID = itemID
	}
	if raw.Item != nil {
		item, err := decodeResponseItem(raw.Item)
		if err != nil {
			return responseEvent{}, err
		}
		event.Item = item
	}
	if raw.Response != nil {
		response, err := decodeResponseEnvelope(raw.Response)
		if err != nil {
			return responseEvent{}, err
		}
		event.Response = response
	}
	if strings.HasSuffix(typeName, ".delta") && raw.Delta == nil {
		return responseEvent{}, invalid("%s requires delta", typeName)
	}
	if typeName == "response.created" && raw.Response == nil {
		return responseEvent{}, invalid("response.created requires response")
	}
	return event, nil
}

func decodeResponseItem(raw any) (responseItem, error) {
	object, ok := raw.(map[string]any)
	if !ok {
		return responseItem{}, invalid("event item must be an object")
	}
	item := responseItem{}
	var err error
	if item.ID, _, err = optionalString(object, "id", "event.item"); err != nil {
		return responseItem{}, err
	}
	if item.Type, _, err = optionalString(object, "type", "event.item"); err != nil {
		return responseItem{}, err
	}
	if item.CallID, _, err = optionalString(object, "call_id", "event.item"); err != nil {
		return responseItem{}, err
	}
	if item.Name, _, err = optionalString(object, "name", "event.item"); err != nil {
		return responseItem{}, err
	}
	if item.Arguments, _, err = optionalString(object, "arguments", "event.item"); err != nil {
		return responseItem{}, err
	}
	return item, nil
}

func decodeResponseEnvelope(raw any) (responseEnvelope, error) {
	object, ok := raw.(map[string]any)
	if !ok {
		return responseEnvelope{}, invalid("event response must be an object")
	}
	response := responseEnvelope{}
	var err error
	if response.ID, _, err = optionalString(object, "id", "event.response"); err != nil {
		return responseEnvelope{}, err
	}
	if response.Model, _, err = optionalString(object, "model", "event.response"); err != nil {
		return responseEnvelope{}, err
	}
	if response.Status, _, err = optionalString(object, "status", "event.response"); err != nil {
		return responseEnvelope{}, err
	}
	if rawUsage, exists := object["usage"]; exists && rawUsage != nil {
		usage, err := decodeResponseUsage(rawUsage)
		if err != nil {
			return responseEnvelope{}, err
		}
		response.Usage = usage
	}
	if rawError, exists := object["error"]; exists && rawError != nil {
		if _, ok := rawError.(map[string]any); !ok {
			return responseEnvelope{}, invalid("event.response.error must be an object")
		}
		response.Error = true
	}
	if rawDetails, exists := object["incomplete_details"]; exists && rawDetails != nil {
		details, ok := rawDetails.(map[string]any)
		if !ok {
			return responseEnvelope{}, invalid("event.response.incomplete_details must be an object")
		}
		reason, _, err := optionalString(details, "reason", "event.response.incomplete_details")
		if err != nil {
			return responseEnvelope{}, err
		}
		response.IncompleteReason = reason
	}
	return response, nil
}

func decodeResponseUsage(raw any) (*responseUsage, error) {
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, invalid("event.response.usage must be an object")
	}
	usage := &responseUsage{}
	var err error
	if usage.InputTokens, err = optionalJSONInt(object, "input_tokens", "event.response.usage"); err != nil {
		return nil, err
	}
	if usage.OutputTokens, err = optionalJSONInt(object, "output_tokens", "event.response.usage"); err != nil {
		return nil, err
	}
	if usage.TotalTokens, err = optionalJSONInt(object, "total_tokens", "event.response.usage"); err != nil {
		return nil, err
	}
	if rawDetails, exists := object["output_tokens_details"]; exists && rawDetails != nil {
		details, ok := rawDetails.(map[string]any)
		if !ok {
			return nil, invalid("event.response.usage.output_tokens_details must be an object")
		}
		if usage.ReasoningTokens, err = optionalJSONInt(details, "reasoning_tokens", "event.response.usage.output_tokens_details"); err != nil {
			return nil, err
		}
	}
	return usage, nil
}

func optionalJSONInt(object map[string]any, key, path string) (int, error) {
	raw, ok := object[key]
	if !ok {
		return 0, nil
	}
	return jsonInt(raw, path+"."+key)
}

func jsonInt(raw any, path string) (int, error) {
	number, ok := raw.(json.Number)
	if !ok {
		return 0, invalid("%s must be an integer", path)
	}
	value, err := number.Int64()
	if err != nil || int64(int(value)) != value {
		return 0, invalid("%s must be an integer", path)
	}
	return int(value), nil
}
