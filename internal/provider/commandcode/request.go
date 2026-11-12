package commandcode

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// defaultMaxTokens mirrors the CLI default the upstream expects.
const defaultMaxTokens = 64000

// buildEnvelope translates an OpenAI-shaped chat body into the CommandCode
// /alpha/generate envelope. The upstream takes Anthropic-style params: system
// text is hoisted out of messages, every message content is an array of typed
// blocks, and tools are plain {name, description, input_schema} objects.
func buildEnvelope(req provider.ChatRequest, threadID string) ([]byte, error) {
	raw, err := req.Body()
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("commandcode: malformed request body: %w", err)
	}

	messages, system := convertMessages(body["messages"])
	params := map[string]any{
		"model":       req.Model,
		"messages":    messages,
		"stream":      true,
		"max_tokens":  maxTokens(body),
		"temperature": temperature(body),
	}
	if system != "" {
		params["system"] = system
	}
	if tools := convertTools(body["tools"]); tools != nil {
		params["tools"] = tools
	}
	if topP, ok := body["top_p"]; ok && topP != nil {
		params["top_p"] = topP
	}
	if effort := reasoningEffort(body); effort != "" {
		params["reasoning_effort"] = effort
	}

	workingDir := workingDir()
	envelope := map[string]any{
		"threadId":       threadID,
		"memory":         nil,
		"taste":          nil,
		"skills":         nil,
		"permissionMode": "standard",
		"config": map[string]any{
			"workingDir":    workingDir,
			"date":          time.Now().UTC().Format("2006-01-02"),
			"environment":   runtime.GOOS,
			"structure":     []any{},
			"isGitRepo":     false,
			"currentBranch": "",
			"mainBranch":    "",
			"gitStatus":     "",
			"recentCommits": []any{},
		},
		"params": params,
	}
	return json.Marshal(envelope)
}

func maxTokens(body map[string]any) int {
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if value, ok := numberAsInt(body[key]); ok && value > 0 {
			return value
		}
	}
	return defaultMaxTokens
}

func temperature(body map[string]any) float64 {
	if value, ok := body["temperature"].(float64); ok {
		return value
	}
	return 0.3
}

func reasoningEffort(body map[string]any) string {
	if effort, ok := body["reasoning_effort"].(string); ok && effort != "" {
		return effort
	}
	if output, ok := body["output_config"].(map[string]any); ok {
		if effort, ok := output["effort"].(string); ok && effort != "" {
			return effort
		}
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			return effort
		}
	}
	return ""
}

// convertMessages splits system text out of the message list and normalizes
// every remaining message to typed content blocks.
func convertMessages(raw any) ([]any, string) {
	items, _ := raw.([]any)
	out := make([]any, 0, len(items))
	systemParts := make([]string, 0, 1)

	toolNames := map[string]string{}
	for _, item := range items {
		message, _ := item.(map[string]any)
		if message == nil {
			continue
		}
		if role, _ := message["role"].(string); role != "assistant" {
			continue
		}
		calls, _ := message["tool_calls"].([]any)
		for _, call := range calls {
			entry, _ := call.(map[string]any)
			if entry == nil {
				continue
			}
			id, _ := entry["id"].(string)
			fn, _ := entry["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if id != "" && name != "" {
				toolNames[id] = name
			}
		}
	}

	for _, item := range items {
		message, _ := item.(map[string]any)
		if message == nil {
			continue
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			if text := flattenText(message["content"]); text != "" {
				systemParts = append(systemParts, text)
			}
		case "tool":
			name, _ := message["name"].(string)
			if name == "" {
				id, _ := message["tool_call_id"].(string)
				name = toolNames[id]
			}
			if name == "" {
				name = "unknown"
			}
			isError := false
			if value, ok := message["is_error"].(bool); ok {
				isError = value
			}
			outputType := "text"
			if isError {
				outputType = "error-text"
			}
			id, _ := message["tool_call_id"].(string)
			out = append(out, map[string]any{
				"role": "tool",
				"content": []any{map[string]any{
					"type":       "tool-result",
					"toolCallId": id,
					"toolName":   name,
					"output":     map[string]any{"type": outputType, "value": flattenText(message["content"])},
				}},
			})
		case "assistant":
			blocks := make([]any, 0, 3)
			if reasoning, ok := message["reasoning_content"].(string); ok && strings.TrimSpace(reasoning) != "" {
				blocks = append(blocks, map[string]any{"type": "reasoning", "text": reasoning})
			}
			if text := flattenText(message["content"]); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			calls, _ := message["tool_calls"].([]any)
			for _, call := range calls {
				entry, _ := call.(map[string]any)
				if entry == nil {
					continue
				}
				fn, _ := entry["function"].(map[string]any)
				id, _ := entry["id"].(string)
				name, _ := fn["name"].(string)
				blocks = append(blocks, map[string]any{
					"type":       "tool-call",
					"toolCallId": id,
					"toolName":   name,
					"input":      safeParseJSON(fn["arguments"]),
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]any{"type": "text", "text": ""})
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		default:
			out = append(out, map[string]any{"role": "user", "content": toContentBlocks(message["content"])})
		}
	}
	return out, strings.Join(systemParts, "\n\n")
}

// toContentBlocks normalizes message content to an array of typed blocks. The
// upstream never accepts a bare string, so text and images both become blocks.
func toContentBlocks(content any) []any {
	switch typed := content.(type) {
	case nil:
		return []any{map[string]any{"type": "text", "text": ""}}
	case string:
		return []any{map[string]any{"type": "text", "text": typed}}
	case []any:
		blocks := make([]any, 0, len(typed))
		for _, part := range typed {
			switch value := part.(type) {
			case string:
				blocks = append(blocks, map[string]any{"type": "text", "text": value})
			case map[string]any:
				if blockType, _ := value["type"].(string); blockType == "text" {
					if text, ok := value["text"].(string); ok {
						blocks = append(blocks, map[string]any{"type": "text", "text": text})
						continue
					}
				}
				if image := imageBlock(value); image != nil {
					blocks = append(blocks, image)
					continue
				}
				if text, ok := value["text"].(string); ok {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			}
		}
		if len(blocks) == 0 {
			return []any{map[string]any{"type": "text", "text": ""}}
		}
		return blocks
	default:
		return []any{map[string]any{"type": "text", "text": flattenText(content)}}
	}
}

// imageBlock maps an OpenAI image_url or Anthropic image source to the native
// image block. Non-data URIs are dropped: the upstream only takes base64.
func imageBlock(part map[string]any) map[string]any {
	if blockType, _ := part["type"].(string); blockType == "image_url" {
		var uri string
		switch value := part["image_url"].(type) {
		case string:
			uri = value
		case map[string]any:
			uri, _ = value["url"].(string)
		}
		if mime, _, ok := parseDataURI(uri); ok {
			return map[string]any{"type": "image", "image": uri, "mimeType": mime, "mediaType": mime}
		}
		return nil
	}
	if blockType, _ := part["type"].(string); blockType == "image" {
		if uri, ok := part["image"].(string); ok && strings.HasPrefix(uri, "data:") {
			mime, _ := part["mimeType"].(string)
			if parsedMime, _, ok := parseDataURI(uri); ok && mime == "" {
				mime = parsedMime
			}
			if mime == "" {
				mime = "image/png"
			}
			return map[string]any{"type": "image", "image": uri, "mimeType": mime, "mediaType": mime}
		}
		if source, ok := part["source"].(map[string]any); ok {
			if kind, _ := source["type"].(string); kind == "base64" {
				mime, _ := source["media_type"].(string)
				if mime == "" {
					mime = "image/png"
				}
				data, _ := source["data"].(string)
				return map[string]any{"type": "image", "image": "data:" + mime + ";base64," + data, "mimeType": mime, "mediaType": mime}
			}
		}
	}
	return nil
}

func parseDataURI(uri string) (string, string, bool) {
	if !strings.HasPrefix(uri, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(uri, "data:")
	header, data, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	mime := strings.TrimSuffix(header, ";base64")
	if mime == "" {
		mime = "image/png"
	}
	return mime, data, true
}

// convertTools maps OpenAI function tools to the Anthropic plain shape.
func convertTools(raw any) []any {
	tools, _ := raw.([]any)
	if len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		if tool == nil {
			continue
		}
		if toolType, _ := tool["type"].(string); toolType == "function" {
			fn, _ := tool["function"].(map[string]any)
			if fn == nil {
				continue
			}
			name, _ := fn["name"].(string)
			schema, ok := fn["parameters"]
			if !ok || schema == nil {
				schema = map[string]any{"type": "object"}
			}
			entry := map[string]any{"name": name, "input_schema": schema}
			if description, ok := fn["description"].(string); ok && description != "" {
				entry["description"] = description
			}
			out = append(out, entry)
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}
		entry := map[string]any{"name": name}
		if description, ok := tool["description"].(string); ok && description != "" {
			entry["description"] = description
		}
		if schema, ok := tool["input_schema"]; ok {
			entry["input_schema"] = schema
		} else if schema, ok := tool["parameters"]; ok {
			entry["input_schema"] = schema
		} else {
			entry["input_schema"] = map[string]any{"type": "object"}
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func safeParseJSON(raw any) any {
	switch value := raw.(type) {
	case nil:
		return map[string]any{}
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return map[string]any{}
		}
		var parsed any
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			return map[string]any{"raw": value}
		}
		return parsed
	default:
		return value
	}
}

func flattenText(content any) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, part := range value {
			switch entry := part.(type) {
			case string:
				parts = append(parts, entry)
			case map[string]any:
				if text, ok := entry["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprint(value)
	}
}

func workingDir() string {
	if dir, err := os.Getwd(); err == nil {
		return dir
	}
	return ""
}
