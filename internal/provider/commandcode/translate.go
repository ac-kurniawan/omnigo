package commandcode

import (
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func openAIToCommandCode(req provider.ChatRequest, model string, threadID string, now func() time.Time) ([]byte, error) {
	if now == nil {
		now = time.Now
	}
	messages := requestMessages(req)
	system, outMsgs, err := translateMessages(messages)
	if err != nil {
		return nil, err
	}
	params := map[string]any{
		"model":       model,
		"messages":    outMsgs,
		"stream":      true,
		"max_tokens":  64000,
		"temperature": 0.3,
	}
	if system != "" {
		params["system"] = system
	}
	if req.Parsed != nil {
		if n, ok := numberValue(req.Parsed["max_tokens"]); ok {
			params["max_tokens"] = int(n)
		} else if n, ok := numberValue(req.Parsed["max_completion_tokens"]); ok {
			params["max_tokens"] = int(n)
		}
		if n, ok := numberValue(req.Parsed["temperature"]); ok {
			params["temperature"] = n
		}
		if n, ok := numberValue(req.Parsed["top_p"]); ok {
			params["top_p"] = n
		}
		if effort, ok := req.Parsed["reasoning_effort"].(string); ok && effort != "" {
			params["reasoning_effort"] = effort
		}
		if tools, ok := translateTools(req.Parsed["tools"]); ok {
			params["tools"] = tools
		}
	}
	cwd, _ := os.Getwd()
	body := map[string]any{
		"threadId":       threadID,
		"memory":         nil,
		"taste":          nil,
		"skills":         nil,
		"permissionMode": "standard",
		"config": map[string]any{
			"workingDir":    cwd,
			"date":          now().Format("2006-01-02"),
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
	return json.Marshal(body)
}

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

func translateMessages(messages []any) (string, []any, error) {
	toolNameByID := buildToolNameMap(messages)
	systemParts := []string{}
	out := make([]any, 0, len(messages))
	started := false
	for i, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("commandcode: messages[%d] must be an object", i)
		}
		role, _ := msg["role"].(string)
		if (role == "system" || role == "developer") && !started {
			if t := contentText(msg["content"]); t != "" {
				systemParts = append(systemParts, t)
			}
			continue
		}
		started = true
		switch role {
		case "system", "developer":
			if t := contentText(msg["content"]); t != "" {
				systemParts = append(systemParts, t)
			}
		case "user":
			blocks := userBlocks(msg["content"])
			if len(blocks) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "user", "content": blocks})
		case "assistant":
			blocks := assistantBlocks(msg)
			if len(blocks) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			id, _ := msg["tool_call_id"].(string)
			if id == "" {
				id, _ = msg["tool_use_id"].(string)
			}
			name := toolNameByID[id]
			if name == "" {
				name = "unknown"
			}
			text := contentText(msg["content"])
			out = append(out, map[string]any{"role": "user", "content": []any{map[string]any{
				"type":       "tool-result",
				"toolCallId": id,
				"toolName":   name,
				"output":     map[string]any{"type": "text", "value": text},
			}}})
		default:
			blocks := userBlocks(msg["content"])
			if len(blocks) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "user", "content": blocks})
		}
	}
	return strings.Join(systemParts, "\n\n"), out, nil
}

func buildToolNameMap(messages []any) map[string]string {
	out := map[string]string{}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		calls, _ := msg["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			id, _ := call["id"].(string)
			fn, _ := call["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if id != "" && name != "" {
				out[id] = name
			}
		}
	}
	return out
}

func userBlocks(content any) []any {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": c}}
	case []any:
		blocks := make([]any, 0, len(c))
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := part["type"].(string)
			switch kind {
			case "text", "input_text":
				text, _ := part["text"].(string)
				if text == "" {
					text = contentText(part["text"])
				}
				if text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			case "image_url":
				if img := imageBlock(part["image_url"]); img != nil {
					blocks = append(blocks, img)
				}
			case "image":
				if img := imageBlock(part); img != nil {
					blocks = append(blocks, img)
				}
			}
		}
		return blocks
	default:
		if t := contentText(c); t != "" {
			return []any{map[string]any{"type": "text", "text": t}}
		}
		return nil
	}
}

func imageBlock(raw any) map[string]any {
	var url string
	switch v := raw.(type) {
	case string:
		url = v
	case map[string]any:
		if u, ok := v["url"].(string); ok {
			url = u
		} else if u, ok := v["image"].(string); ok {
			url = u
		} else if u, ok := v["data"].(string); ok {
			url = u
		}
	}
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	comma := strings.Index(url, ",")
	if comma < 0 {
		return nil
	}
	meta := url[5:comma]
	data := url[comma+1:]
	mt := "image/png"
	if parts := strings.Split(meta, ";"); len(parts) > 0 && parts[0] != "" {
		if _, _, err := mime.ParseMediaType(parts[0]); err == nil || strings.Contains(parts[0], "/") {
			mt = parts[0]
		}
	}
	return map[string]any{"type": "image", "image": data, "mimeType": mt, "mediaType": mt}
}

func assistantBlocks(msg map[string]any) []any {
	blocks := []any{}
	if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
		blocks = append(blocks, map[string]any{"type": "reasoning", "text": rc})
	}
	if text := contentText(msg["content"]); text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	if calls, ok := msg["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			name, _ := fn["name"].(string)
			id, _ := call["id"].(string)
			args := callArgs(fn["arguments"])
			blocks = append(blocks, map[string]any{
				"type": "tool-call", "toolCallId": id, "toolName": name, "input": args,
			})
		}
	}
	return blocks
}

func callArgs(raw any) any {
	switch v := raw.(type) {
	case string:
		if v == "" {
			return map[string]any{}
		}
		var out any
		if err := json.Unmarshal([]byte(v), &out); err == nil {
			return out
		}
		return map[string]any{}
	case map[string]any:
		return v
	case nil:
		return map[string]any{}
	default:
		return v
	}
}

func contentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := part["type"].(string)
			if kind == "text" || kind == "input_text" || kind == "reasoning" {
				if text, ok := part["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

func translateTools(raw any) ([]any, bool) {
	tools, ok := raw.([]any)
	if !ok || len(tools) == 0 {
		return nil, false
	}
	out := make([]any, 0, len(tools))
	seen := map[string]struct{}{}
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := tool["type"].(string)
		if kind != "" && kind != "function" {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		decl := map[string]any{"name": name}
		if desc, _ := fn["description"].(string); desc != "" {
			decl["description"] = desc
		}
		if schema, ok := fn["parameters"].(map[string]any); ok && len(schema) > 0 {
			decl["input_schema"] = schema
		} else {
			decl["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, decl)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func numberValue(raw any) (float64, bool) {
	switch n := raw.(type) {
	case json.Number:
		v, err := n.Float64()
		return v, err == nil
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
