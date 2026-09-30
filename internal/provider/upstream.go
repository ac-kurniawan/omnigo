package provider

import (
	"io"
	"log"
)

// maxUpstreamSnippetBytes caps how much of an error body is read for
// diagnostics. The error text is a small JSON status; a larger read only
// risks a hostile upstream filling memory on an error path.
const maxUpstreamSnippetBytes = 4096

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
