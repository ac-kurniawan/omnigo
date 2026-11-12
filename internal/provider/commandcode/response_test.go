package commandcode

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestTranslateNDJSONStreamChunks(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"type":"text-delta","text":"hello"}`,
		`{"type":"reasoning-delta","text":"think"}`,
		`{"type":"tool-call","toolCall":{"id":"call-1","name":"get_weather","input":{"city":"x"}}}`,
		`{"type":"tool-input-delta","toolCall":{"id":"call-1"},"delta":{"text":"!tail"}}`,
		`{"type":"finish","item":{"finish_reason":"tool_calls","usage":{"input_tokens":10,"output_tokens":5}}}`,
		``,
		`[DONE]`,
	}, "\n")
	var buf bytes.Buffer
	seen := false
	ctx := context.Background()
	usage, err := translateNDJSON(ctx, strings.NewReader(ndjson), "m", "acct", true, &buf, &seen)
	if err != nil {
		t.Fatalf("translateNDJSON: %v", err)
	}
	if !usage.Present || usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Fatalf("usage = %#v", usage)
	}
	out := buf.String()
	for _, want := range []string{`"content":"hello"`, `"reasoning_content":"think"`, `"get_weather"`, `"finish_reason":"tool_calls"`, `"usage"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream output missing %s:\n%s", want, out)
		}
	}
}

func TestTranslateNDJSONErrorMapping(t *testing.T) {
	cases := []struct {
		msg  string
		want int
	}{
		{`{"type":"error","error":{"message":"unauthorized key"}}`, 401},
		{`{"type":"error","error":{"message":"billing required"}}`, 402},
		{`{"type":"error","error":{"message":"quota exhausted"}}`, 403},
		{`{"type":"error","error":{"message":"model not found"}}`, 404},
		{`{"type":"error","error":{"message":"rate limited"}}`, 429},
		{`{"type":"error","error":{"message":"boom"}}`, 502},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		seen := false
		_, err := translateNDJSON(context.Background(), strings.NewReader(c.msg+"\n"), "m", "a", true, &buf, &seen)
		he, ok := err.(*provider.HTTPStatusError)
		if !ok {
			t.Fatalf("msg %q: err type = %T (%v)", c.msg, err, err)
		}
		if he.StatusCode != c.want {
			t.Fatalf("msg %q: status = %d, want %d", c.msg, he.StatusCode, c.want)
		}
	}
}

func TestTranslateNDJSONAggregatesNonStream(t *testing.T) {
	ndjson := "{\"type\":\"text-delta\",\"text\":\"hi\"}\n{\"type\":\"finish\"}\n"
	var buf bytes.Buffer
	seen := false
	_, err := translateNDJSON(context.Background(), strings.NewReader(ndjson), "m", "a", false, &buf, &seen)
	if err != nil {
		t.Fatalf("translateNDJSON: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("non-stream must not write chunks, got %q", buf.String())
	}
	_ = json.Marshal
}
