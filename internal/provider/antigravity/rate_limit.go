package antigravity

import (
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
}

func (e *rateLimitError) Error() string {
	return "antigravity: rate limited"
}

func (e *rateLimitError) Cooldown() time.Duration {
	return e.cooldown
}

func (e *rateLimitError) DrainReason() string {
	return e.Error()
}

func newRateLimitError(headers http.Header) error {
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
	return &rateLimitError{cooldown: cooldown}
}
