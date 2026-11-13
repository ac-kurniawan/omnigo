package opencode

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// A keepalive line between real frames must not break a translated stream: the
// shared reader skips it, so the translator sees only events it can decode.
func TestStreamToleratesKeepaliveFrames(t *testing.T) {
	feed := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":5}}}`,
		"",
		"data:   ",
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	dst := httptest.NewRecorder()
	if err := streamAnthropicToChat(context.Background(), strings.NewReader(feed), "m", "acct", dst); err != nil {
		t.Fatalf("streamAnthropicToChat: %v", err)
	}
	if !strings.Contains(dst.Body.String(), `"hello"`) {
		t.Fatalf("translated stream lost the delta: %q", dst.Body.String())
	}
}
