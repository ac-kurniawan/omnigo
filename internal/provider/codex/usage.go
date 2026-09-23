package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/quota"
)

const (
	whamUsagePath = "/wham/usage"
	apiUsagePath  = "/api/codex/usage"
	// maxUsageBytes bounds how much of a usage body is decoded.
	maxUsageBytes = 1 << 20
)

// quotaWindows are the windows Codex reports, in upstream order.
var quotaWindows = []string{"primary", "secondary"}

// quotaURLFor derives the usage endpoint from the provider's configured base
// URL, which in this repo is the inference endpoint (see DefaultResponsesURL).
//
// ChatGPT's backend-api serves /wham/usage; any other base is assumed to be an
// API-style deployment serving /api/codex/usage. The inference path is stripped
// first so a configured .../backend-api/codex/responses resolves to
// .../backend-api/wham/usage rather than appending to the responses path. An
// empty base falls back to DefaultResponsesURL, so the default deployment
// polls the ChatGPT backend-api.
func quotaURLFor(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = DefaultResponsesURL
	}
	for _, suffix := range []string{"/codex/responses", "/responses"} {
		if trimmed := strings.TrimSuffix(base, suffix); trimmed != base {
			base = trimmed
			break
		}
	}
	if strings.Contains(base, "/backend-api") {
		return base + whamUsagePath
	}
	return base + apiUsagePath
}

// codexUsage is the decoded usage payload. It models every field verified live
// on 2026-09-18 so the dashboard modal can render provider-native detail from
// AccountSnapshot.Raw without this package inventing a second schema.
type codexUsage struct {
	UserID               string                     `json:"user_id,omitempty"`
	AccountID            string                     `json:"account_id,omitempty"`
	Email                string                     `json:"email,omitempty"`
	PlanType             string                     `json:"plan_type,omitempty"`
	RateLimit            *codexRateLimit            `json:"rate_limit,omitempty"`
	ModelUsage           map[string]codexModelQuota `json:"model_usage,omitempty"`
	Credits              *codexCredits              `json:"credits,omitempty"`
	SpendControl         *codexSpendControl         `json:"spend_control,omitempty"`
	RateLimitReachedType any                        `json:"rate_limit_reached_type,omitempty"`
}

type codexRateLimit struct {
	Allowed         bool         `json:"allowed"`
	LimitReached    bool         `json:"limit_reached"`
	PrimaryWindow   *codexWindow `json:"primary_window,omitempty"`
	SecondaryWindow *codexWindow `json:"secondary_window,omitempty"`
}

type codexWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int     `json:"limit_window_seconds,omitempty"`
	ResetAfterSeconds  int     `json:"reset_after_seconds,omitempty"`
	ResetAt            int64   `json:"reset_at,omitempty"`
}

type codexModelQuota struct {
	Available          bool `json:"available"`
	CreditsWouldEnable bool `json:"credits_would_enable,omitempty"`
	AvailableAt        any  `json:"available_at,omitempty"`
}

type codexCredits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

type codexSpendControl struct {
	Reached bool `json:"reached"`
}

// snapshotWindow converts one upstream window. Missing window metadata stays
// zero so the dashboard renders "unknown" rather than a fabricated reset.
func (w *codexWindow) snapshotWindow(name string) quota.Window {
	window := quota.Window{Name: name, UsedPercent: w.UsedPercent}
	if w.LimitWindowSeconds > 0 {
		window.WindowMinutes = w.LimitWindowSeconds / 60
	}
	if w.ResetAt > 0 {
		window.ResetAt = time.Unix(w.ResetAt, 0).UTC()
	}
	if w.ResetAfterSeconds > 0 {
		window.ResetAfter = time.Duration(w.ResetAfterSeconds) * time.Second
	}
	return window
}

func (r *codexRateLimit) snapshotWindows() []quota.Window {
	windows := make([]quota.Window, 0, len(quotaWindows))
	for i, name := range quotaWindows {
		var window *codexWindow
		if i == 0 {
			window = r.PrimaryWindow
		} else {
			window = r.SecondaryWindow
		}
		if window == nil {
			continue
		}
		windows = append(windows, window.snapshotWindow(name))
	}
	return windows
}

// reachedType returns the upstream's own exhaustion label when it is a string.
func (u *codexUsage) reachedType() string {
	if label, ok := u.RateLimitReachedType.(string); ok {
		return label
	}
	return ""
}

// FetchQuota reads remaining quota for one account from the Codex usage
// endpoint.
//
// Exhaustion is keyed strictly on the upstream's explicit signal:
// rate_limit.allowed == false or rate_limit.limit_reached == true. A window at
// used_percent == 100 with allowed == true is a live-observed state in which the
// account keeps serving, so it MUST classify as StatusAvailable; keying on
// used_percent would drain healthy credentials. Every failure yields
// StatusUnavailable, which never drains.
func (p *Provider) FetchQuota(ctx context.Context, account provider.Credentials) (quota.AccountSnapshot, error) {
	snapshot := quota.AccountSnapshot{
		Provider:   p.name,
		Identity:   account.Identity(),
		AccountID:  account.AccountID,
		Email:      account.Email,
		ObservedAt: timeNow(),
	}
	creds, err := p.tokenManager(account).EnsureFreshToken(ctx)
	if err != nil {
		return unavailable(snapshot, "authentication failed",
			fmt.Errorf("codex: quota credentials: %w", err))
	}
	token := creds.AccessToken
	if token == "" {
		token = account.AccessToken
	}
	accountID := creds.AccountID
	if accountID == "" {
		accountID = account.AccountID
	}
	if accountID == "" {
		return unavailable(snapshot, "account id missing", fmt.Errorf("codex: account ID is missing"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.quotaURL, nil)
	if err != nil {
		return unavailable(snapshot, "invalid usage endpoint",
			fmt.Errorf("codex: create quota request: %w", err))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("Version", ClientVersion)
	req.Header.Set("originator", Originator)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("OpenAI-Beta", BetaVersion)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return unavailable(snapshot, "upstream unreachable", fmt.Errorf("codex: quota request failed: %w", err))
	}
	defer resp.Body.Close()
	// A plain error, not provider.NewHTTPStatusError: quota reads are
	// display-only and must never look drainable to the account pool.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		reason := fmt.Sprintf("upstream status %d", resp.StatusCode)
		return unavailable(snapshot, reason, fmt.Errorf("codex: quota %s", reason))
	}
	var payload codexUsage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUsageBytes)).Decode(&payload); err != nil {
		return unavailable(snapshot, "invalid quota response", fmt.Errorf("codex: decode quota response: %w", err))
	}
	if payload.RateLimit == nil {
		return unavailable(snapshot, "no rate limit data", fmt.Errorf("codex: quota response carried no rate limit"))
	}
	snapshot.AccountID = firstNonEmpty(payload.AccountID, account.AccountID)
	snapshot.Email = firstNonEmpty(payload.Email, account.Email)
	snapshot.PlanType = payload.PlanType
	snapshot.Raw = &payload
	snapshot.Windows = payload.RateLimit.snapshotWindows()
	if !payload.RateLimit.Allowed || payload.RateLimit.LimitReached {
		snapshot.Status = quota.StatusExhausted
		snapshot.Reason = payload.reachedType()
	} else {
		snapshot.Status = quota.StatusAvailable
	}
	return snapshot, nil
}

// unavailable marks a snapshot display-only, never draining, and pairs it with
// the error the caller should log. Reason stays a fixed short string so no
// upstream body or credential can reach the dashboard.
func unavailable(snapshot quota.AccountSnapshot, reason string, err error) (quota.AccountSnapshot, error) {
	snapshot.Status = quota.StatusUnavailable
	snapshot.Reason = reason
	return snapshot, err
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// QuotaFromHeaders builds a snapshot from the quota metadata that ordinary
// inference responses carry (x-codex-<window>-...).
//
// It is meaningful only for a successful response: the headers carry no
// allowed/limit_reached signal, and upstream serves normally with a window at
// used_percent == 100, so a parsed header set classifies as StatusAvailable.
// ok is false when the response carried no quota metadata at all, which leaves
// the previously cached snapshot untouched.
func (p *Provider) QuotaFromHeaders(account provider.Credentials, h http.Header) (quota.AccountSnapshot, bool) {
	if h == nil {
		return quota.AccountSnapshot{}, false
	}
	windows := make([]quota.Window, 0, len(quotaWindows))
	for _, name := range quotaWindows {
		prefix := "x-codex-" + name + "-"
		used, err := strconv.ParseFloat(h.Get(prefix+"used-percent"), 64)
		if err != nil {
			continue
		}
		window := quota.Window{Name: name, UsedPercent: used}
		if minutes, err := strconv.Atoi(h.Get(prefix + "window-minutes")); err == nil && minutes > 0 {
			window.WindowMinutes = minutes
		}
		if resetAt, err := strconv.ParseInt(h.Get(prefix+"reset-at"), 10, 64); err == nil && resetAt > 0 {
			window.ResetAt = time.Unix(resetAt, 0).UTC()
		}
		windows = append(windows, window)
	}
	if len(windows) == 0 {
		return quota.AccountSnapshot{}, false
	}
	return quota.AccountSnapshot{
		Provider:   p.name,
		Identity:   account.Identity(),
		AccountID:  account.AccountID,
		Email:      account.Email,
		Status:     quota.StatusAvailable,
		ObservedAt: timeNow(),
		Windows:    windows,
	}, true
}

// SetQuotaObserver registers a callback invoked with a fresh snapshot whenever
// a successful inference response carries quota headers. The syncer uses it to
// update its cache from live traffic without waiting for the next poll. A nil
// callback clears the registration.
func (p *Provider) SetQuotaObserver(observer func(quota.AccountSnapshot)) {
	p.quotaMu.Lock()
	p.quotaObserver = observer
	p.quotaMu.Unlock()
}

// CapturedQuota returns the most recent quota snapshot observed for the given
// account, if any.
func (p *Provider) CapturedQuota(account provider.Credentials) (quota.AccountSnapshot, bool) {
	p.quotaMu.RLock()
	defer p.quotaMu.RUnlock()
	if p.capturedQuota == nil {
		return quota.AccountSnapshot{}, false
	}
	snap, ok := p.capturedQuota[account.Identity()]
	return snap, ok
}

// observeQuota records live inference quota into capturedQuota and notifies the
// registered observer, if any.
func (p *Provider) observeQuota(account provider.Credentials, h http.Header) {
	snapshot, ok := p.QuotaFromHeaders(account, h)
	if !ok {
		return
	}
	p.quotaMu.Lock()
	if p.capturedQuota == nil {
		p.capturedQuota = make(map[string]quota.AccountSnapshot)
	}
	p.capturedQuota[account.Identity()] = snapshot
	observer := p.quotaObserver
	p.quotaMu.Unlock()
	if observer != nil {
		observer(snapshot)
	}
}

// MarkQuotaDrained cools one credential in this provider's account pool
// because its upstream quota is exhausted. It lets the syncer pre-drain an
// account through the same pool the request path uses, without exposing the
// pool itself.
func (p *Provider) MarkQuotaDrained(identity string, cooldown time.Duration, reason string) {
	p.pool.MarkQuotaDrained(identity, cooldown, reason)
}
