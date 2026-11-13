package provider

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// sseDoneSentinel ends an OpenAI-shaped stream. ReadSSE consumes it instead of
// handing it to the caller's decoder.
const sseDoneSentinel = "[DONE]"

// MaxSSELine is the longest SSE line a scan accepts. A reasoning delta arrives
// as one line and a long one runs past a few megabytes; a smaller bound ends the
// stream with "token too long" wherever that delta happened to be.
const MaxSSELine = 50 * 1024 * 1024

// ReadSSE walks an SSE feed and hands each complete event to consume with its
// event type (empty when the feed names none) and its data payload. A comment
// line is skipped, multiple data lines join with a newline, and the [DONE]
// sentinel is consumed here rather than passed on. label names the provider in
// a read error.
//
// A translator that re-frames an upstream stream uses this so framing behaves
// the same on every wire.
func ReadSSE(ctx context.Context, reader io.Reader, label string, consume func(eventType string, data []byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), MaxSSELine)
	var eventType string
	var data strings.Builder
	flush := func() error {
		// A whitespace-only data field is a keepalive, not a payload: upstreams
		// send bare "data:" lines to hold the connection open, and handing that
		// to a decoder would fail the whole stream as malformed.
		if strings.TrimSpace(data.String()) == "" {
			data.Reset()
			eventType = ""
			return nil
		}
		payload := data.String()
		typeName := eventType
		data.Reset()
		eventType = ""
		if strings.TrimSpace(payload) == sseDoneSentinel {
			return nil
		}
		return consume(typeName, []byte(payload))
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		switch field {
		case "event":
			eventType = strings.TrimSpace(value)
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			// The protocol removes exactly one space after the colon; the rest
			// of the line is payload and reaches the decoder untouched.
			data.WriteString(strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: read SSE stream: %w", label, err)
	}
	return flush()
}

// WriteSSEChunk writes one JSON chunk as an SSE data frame.
func WriteSSEChunk(w io.Writer, chunk map[string]any) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

// First returns the first non-nil value, or nil when every value is nil.
func First(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// FirstPresent returns the first present, non-nil field named by keys.
func FirstPresent(object map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, ok := object[key]; ok && value != nil {
			return value, true
		}
	}
	return nil, false
}

// NumberAsInt reads a JSON number as an int. Every caller reads a count or an
// index, so a negative value and any non-numeric type report false.
func NumberAsInt(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		if number < 0 {
			return 0, false
		}
		return int(number), true
	case json.Number:
		parsed, err := number.Int64()
		if err != nil || parsed < 0 {
			return 0, false
		}
		return int(parsed), true
	case int:
		return number, number >= 0
	case int64:
		return int(number), number >= 0
	default:
		return 0, false
	}
}

// RandomID returns 12 random bytes as hex, for a synthesized id. A failing
// entropy source falls back to the clock rather than an empty id.
func RandomID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(value)
}
