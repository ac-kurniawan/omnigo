package codex

import (
	"net/http"
	"strconv"
	"time"
)

const (
	minQuotaCooldown     = time.Minute
	defaultQuotaCooldown = 5 * time.Minute
	maxQuotaCooldown     = 30 * time.Minute
)

var timeNow = time.Now

type quotaError struct {
	cooldown time.Duration
}

func (e *quotaError) Error() string {
	return "codex: quota exhausted"
}

func (e *quotaError) Cooldown() time.Duration {
	return e.cooldown
}

func (e *quotaError) DrainReason() string {
	return e.Error()
}

func (e *quotaError) QuotaExhausted() bool { return true }

func newQuotaError(headers http.Header) error {
	cooldown := quotaCooldown(headers, timeNow())
	return &quotaError{cooldown: cooldown}
}

func quotaCooldown(headers http.Header, now time.Time) time.Duration {
	var cooldown time.Duration
	for _, window := range []string{"primary", "secondary"} {
		used, err := strconv.ParseFloat(headers.Get("x-codex-"+window+"-used-percent"), 64)
		if err != nil || used < 100 {
			continue
		}
		resetAt, err := strconv.ParseInt(headers.Get("x-codex-"+window+"-reset-at"), 10, 64)
		if err != nil {
			continue
		}
		untilReset := time.Unix(resetAt, 0).Sub(now)
		if untilReset > cooldown {
			cooldown = untilReset
		}
	}
	if cooldown <= 0 {
		if seconds, err := strconv.ParseInt(headers.Get("Retry-After"), 10, 64); err == nil && seconds > 0 {
			cooldown = time.Duration(seconds) * time.Second
		}
	}
	if cooldown <= 0 {
		cooldown = defaultQuotaCooldown
	}
	if cooldown < minQuotaCooldown {
		return minQuotaCooldown
	}
	if cooldown > maxQuotaCooldown {
		return maxQuotaCooldown
	}
	return cooldown
}
