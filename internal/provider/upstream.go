package provider

import (
	"fmt"
	"io"
	"log"
)

// maxUpstreamSnippetBytes caps how much of an error body is read for
// diagnostics. The error text is a small JSON status; a larger read only
// risks a hostile upstream filling memory on an error path.
const maxUpstreamSnippetBytes = 4096

// MaxUnaryBodyBytes bounds a metadata response read whole: a models catalog or
// a quota snapshot is kilobytes, and reading one without a limit lets a hostile
// or broken upstream decide this process's memory use.
const MaxUnaryBodyBytes = 1 << 20

// ReadBoundedBody reads at most MaxUnaryBodyBytes and reports the oversized
// read as an error rather than truncating the JSON a caller is about to decode.
func ReadBoundedBody(r io.Reader) ([]byte, error) {
	return readBounded(r, MaxUnaryBodyBytes)
}

// MaxCompletionBodyBytes bounds a whole non-streaming completion body. It is far
// larger than a metadata response because the body is the answer itself, and
// still far smaller than what a broken upstream could send.
const MaxCompletionBodyBytes = 8 << 20

// ReadCompletionBody reads a whole non-streaming completion. A body past the
// cap is an error: truncating it would hand the decoder malformed JSON.
func ReadCompletionBody(r io.Reader) ([]byte, error) {
	return readBounded(r, MaxCompletionBodyBytes)
}

func readBounded(r io.Reader, limit int) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("upstream body exceeds %d bytes", limit)
	}
	return raw, nil
}

// UpstreamSnippet drains a bounded prefix of an error body for logging and
// for client-facing rate-limit messages.
func UpstreamSnippet(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, maxUpstreamSnippetBytes))
	if err != nil {
		return ""
	}
	return string(raw)
}

// WarnUpstreamError logs one warning per upstream 4xx/5xx response. The
// snippet is redacted because upstream error text can embed reflected
// credentials, and RedactCredentials flattens whitespace so a hostile body
// cannot forge extra log lines.
func WarnUpstreamError(providerName, model, account string, status int, snippet string) {
	log.Printf("[warn] upstream status=%d provider=%q model=%q account=%q message=%q",
		status, providerName, model, account, RedactCredentials(snippet))
}
