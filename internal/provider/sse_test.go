package provider

import (
	"context"
	"strings"
	"testing"
)

type sseEvent struct {
	typ  string
	data string
}

func drainSSE(t *testing.T, feed string) ([]sseEvent, error) {
	t.Helper()
	var got []sseEvent
	err := ReadSSE(context.Background(), strings.NewReader(feed), "test", func(typ string, data []byte) error {
		got = append(got, sseEvent{typ: typ, data: string(data)})
		return nil
	})
	return got, err
}

func TestReadSSEFramesEvents(t *testing.T) {
	feed := strings.Join([]string{
		": comment",
		"event: response.output_text.delta",
		`data: {"a": 1}`,
		"",
		"data: first",
		"data: second",
		"",
		"data: [DONE]",
		"",
	}, "\n")
	got, err := drainSSE(t, feed)
	if err != nil {
		t.Fatalf("ReadSSE: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("events = %+v, want 2", got)
	}
	if got[0].typ != "response.output_text.delta" || got[0].data != `{"a": 1}` {
		t.Fatalf("first event = %+v", got[0])
	}
	// A multi-line data field joins with a newline; [DONE] never reaches the
	// decoder.
	if got[1].typ != "" || got[1].data != "first\nsecond" {
		t.Fatalf("second event = %+v", got[1])
	}
}

// Framing belongs to the reader, so a data line reaches the decoder byte for
// byte after the single protocol space: both providers decode the same JSON.
func TestReadSSEPreservesPayloadWhitespace(t *testing.T) {
	got, err := drainSSE(t, "data:   {\"k\": \"v\"}  \n\n")
	if err != nil {
		t.Fatalf("ReadSSE: %v", err)
	}
	if len(got) != 1 || got[0].data != `  {"k": "v"}  ` {
		t.Fatalf("events = %+v", got)
	}
}

func TestReadSSEStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ReadSSE(ctx, strings.NewReader("data: x\n\n"), "test", func(string, []byte) error {
		t.Fatal("consume ran on a cancelled context")
		return nil
	})
	if err == nil {
		t.Fatal("cancelled read reported no error")
	}
}

// A bare "data:" line is a keepalive some upstreams send while a model thinks.
// It must not reach a decoder, which would fail the stream as malformed.
func TestReadSSESkipsWhitespaceOnlyData(t *testing.T) {
	feed := "data:   \n\n" + "data: {\"a\":1}\n\n"
	got, err := drainSSE(t, feed)
	if err != nil {
		t.Fatalf("ReadSSE: %v", err)
	}
	if len(got) != 1 || got[0].data != `{"a":1}` {
		t.Fatalf("events = %+v, want only the JSON frame", got)
	}
}
