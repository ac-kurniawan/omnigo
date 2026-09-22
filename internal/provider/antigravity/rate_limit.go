package antigravity

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

const (
	minRateLimitCooldown     = time.Minute
	defaultRateLimitCooldown = 5 * time.Minute
	maxRateLimitCooldown     = 30 * time.Minute
)

type rateLimitError struct {
	cooldown time.Duration
	detail   string
}

func (e *rateLimitError) Error() string {
	if e.detail == "" {
		return "antigravity: rate limited"
	}
	return "antigravity: rate limited: " + e.detail
}

func (e *rateLimitError) Cooldown() time.Duration {
	return e.cooldown
}

func (e *rateLimitError) DrainReason() string {
	return e.Error()
}

// HTTPStatus and RetryAfter let writeProviderError answer the client with a
// real 429 and Retry-After instead of collapsing an upstream rate limit into
// a generic 502.
func (e *rateLimitError) HTTPStatus() int { return http.StatusTooManyRequests }

func (e *rateLimitError) RetryAfter() time.Duration { return e.cooldown }

func newRateLimitError(headers http.Header, detail string) error {
	cooldown := defaultRateLimitCooldown
	if seconds, err := strconv.ParseInt(headers.Get("Retry-After"), 10, 64); err == nil && seconds > 0 {
		if seconds >= int64(maxRateLimitCooldown/time.Second) {
			cooldown = maxRateLimitCooldown
		} else {
			cooldown = time.Duration(seconds) * time.Second
		}
	}
	if cooldown < minRateLimitCooldown {
		cooldown = minRateLimitCooldown
	}
	if cooldown > maxRateLimitCooldown {
		cooldown = maxRateLimitCooldown
	}
	return &rateLimitError{cooldown: cooldown, detail: upstreamRateLimitDetail(detail)}
}

// Upstream 429 bodies are untrusted. Only status and reason identifiers from
// Google's structured error envelope are exposed; message text and arbitrary
// fields may contain reflected request data, credentials, or PII.
type upstreamRateLimitEnvelope struct {
	Error struct {
		Status string `json:"status"`
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

func upstreamRateLimitDetail(raw string) string {
	var envelopes []upstreamRateLimitEnvelope
	if err := json.Unmarshal([]byte(raw), &envelopes); err != nil {
		var envelope upstreamRateLimitEnvelope
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			return ""
		}
		envelopes = []upstreamRateLimitEnvelope{envelope}
	}
	if len(envelopes) == 0 {
		return ""
	}
	status := knownRateLimitStatus(envelopes[0].Error.Status)
	reason := ""
	if len(envelopes[0].Error.Errors) > 0 {
		reason = knownRateLimitReason(envelopes[0].Error.Errors[0].Reason)
	}
	switch {
	case status != "" && reason != "":
		return status + " (reason: " + reason + ")"
	case status != "":
		return status
	case reason != "":
		return "reason: " + reason
	default:
		return ""
	}
}

func knownRateLimitStatus(value string) string {
	if value == "RESOURCE_EXHAUSTED" {
		return value
	}
	return ""
}

func knownRateLimitReason(value string) string {
	switch value {
	case "rateLimitExceeded", "quotaExceeded", "userRateLimitExceeded":
		return value
	default:
		return ""
	}
}
