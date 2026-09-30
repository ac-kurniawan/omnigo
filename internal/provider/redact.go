package provider

import (
	"regexp"
	"strings"
)

// credentialPattern matches secrets that appear as key=value or key: "value"
// pairs. Upstream errors embed credentials inside URL query strings and JSON
var credentialPattern = regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|key|token|secret|password|authorization|bearer)["']?\s*[:=]\s*["']?)([^\s"'&,;)}\]]+)`)

// userInfoPattern matches the password half of a URL's userinfo component,
// e.g. https://user:secret@host/.
var userInfoPattern = regexp.MustCompile(`(://[^/\s:@]+:)([^@\s/]+)(@)`)

// RedactCredentials replaces credential-shaped substrings — key=value pairs,
// URL userinfo, Bearer tokens, and sk- style prefixes — with [redacted]. The
// same redaction guards client-facing error messages and operator logs:
// upstream error text can embed reflected credentials.
func RedactCredentials(msg string) string {
	// Redact key=value / key: "value" pairs wherever they appear, so a secret
	// buried in a URL query or a JSON blob is caught along with the plain
	// "Bearer <token>" shape.
	msg = credentialPattern.ReplaceAllString(msg, "${1}[redacted]")
	msg = userInfoPattern.ReplaceAllString(msg, "${1}[redacted]${3}")

	fields := strings.Fields(msg)
	redactNext := false
	for i, field := range fields {
		trimmed := strings.Trim(field, `"'(),;`)
		lower := strings.ToLower(trimmed)
		if redactNext || strings.HasPrefix(lower, "sk-") {
			fields[i] = "[redacted]"
			redactNext = false
			continue
		}
		redactNext = lower == "bearer"
	}
	return strings.Join(fields, " ")
}
