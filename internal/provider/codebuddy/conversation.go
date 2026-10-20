package codebuddy

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// The CodeBuddy international gateway (codebuddy.ai) verifies the calling
// channel on every chat request. A request missing the X-Conversation-* header
// family the official IDE sends is rejected with HTTP 400 code 11128
// ("request illegal", displayTips: "This call failed channel verification").
// The CN endpoint (copilot.tencent.com) is not gated and must not send them.
//
// All three headers must be present. Their values are not validated upstream:
// non-UUID values are accepted, and the message id becomes the id of the
// streamed completion.

// conversationIDs holds the header values for one upstream chat request.
type conversationIDs struct {
	conversationID string
	requestID      string
	messageID      string
}

// conversationIDs derives the X-Conversation-* header values for req.
//
// The conversation id is stable for a session so upstream can reuse its prompt
// cache across the turns of one conversation; the request and message ids are
// fresh for every upstream request.
func (p *codebuddyProvider) conversationIDs(ctx context.Context, req provider.ChatRequest) (conversationIDs, error) {
	key := conversationKey(ctx, req)
	conversationID, err := p.stableConversationID(key)
	if err != nil {
		return conversationIDs{}, err
	}
	requestID, err := randomUUID()
	if err != nil {
		return conversationIDs{}, err
	}
	messageID, err := randomUUID()
	if err != nil {
		return conversationIDs{}, err
	}
	return conversationIDs{conversationID: conversationID, requestID: requestID, messageID: messageID}, nil
}

// conversationKey picks the most stable key available for the conversation:
// an explicit session id from the client, then the opening user message.
// It returns "" when nothing is derivable, which makes the caller fall back
// to a random conversation id.
func conversationKey(ctx context.Context, req provider.ChatRequest) string {
	// The parsed body is read, never mutated, so it needs no copy. Only the
	// raw fallback has to be decoded.
	payload := req.Parsed
	if payload == nil {
		decoded, err := requestPayload(req)
		if err != nil {
			return ""
		}
		payload = decoded
	}

	// Scope the key by the gateway tenant so two clients sending the same
	// opening message or the same session id do not share an upstream prompt
	// cache bucket.
	var prefix string
	if caller := auth.CallerFrom(ctx); caller != nil && caller.ID() != "" {
		prefix = "t:" + caller.ID() + ":"
	}

	for _, field := range []string{"session_id", "prompt_cache_key", "conversation_id"} {
		if value, ok := payload[field].(string); ok && value != "" {
			return prefix + value
		}
	}
	messages, _ := payload["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role != "user" {
			continue
		}
		if text := flattenContent(message["content"]); text != "" {
			return prefix + "cbai:" + text
		}
	}
	return ""
}

// requestPayload decodes the request body without mutating the shared parsed
// map.
func requestPayload(req provider.ChatRequest) (map[string]any, error) {
	if req.Parsed != nil {
		payload := make(map[string]any, len(req.Parsed))
		for k, v := range req.Parsed {
			if k == provider.OpenAIBodyCacheKey() {
				continue
			}
			payload[k] = v
		}
		return payload, nil
	}
	payload := map[string]any{}
	if len(req.Raw) == 0 {
		return payload, nil
	}
	if err := json.Unmarshal(req.Raw, &payload); err != nil {
		return nil, fmt.Errorf("decode request body: %w", err)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return payload, nil
}

// stableConversationID derives the conversation id for key. The key is mixed
// with a per-process salt so the id cannot be reversed to the opening
// message by anyone who can read the id, including other tenants sharing the
// upstream account.
func (p *codebuddyProvider) stableConversationID(key string) (string, error) {
	if key == "" {
		return randomUUID()
	}
	return stableUUID(p.salt + "\x00" + key)
}

// stableUUID derives an RFC 4122 v5-shaped UUID from key, so the same key
// always yields the same id. An empty key yields a random id.
func stableUUID(key string) (string, error) {
	if key == "" {
		return randomUUID()
	}
	sum := sha1.Sum([]byte(key))
	hexed := hex.EncodeToString(sum[:])
	version := (parseHex(hexed[12:16]) & 0x0fff) | 0x5000
	variant := (parseHex(hexed[16:20]) & 0x3fff) | 0x8000
	return strings.Join([]string{
		hexed[0:8],
		hexed[8:12],
		fmt.Sprintf("%04x", version),
		fmt.Sprintf("%04x", variant),
		hexed[20:32],
	}, "-"), nil
}

// randomUUID returns a random RFC 4122 v4 UUID.
func randomUUID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

// randomSalt returns a random hex string used to salt the derived
// conversation id.
func randomSalt() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// parseHex parses a hex slice, returning 0 on malformed input.
func parseHex(s string) int {
	value, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0
	}
	return int(value)
}
