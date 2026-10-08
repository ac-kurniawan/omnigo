package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ClientID             = "1031625952748946"
	DefaultDeviceAuthURL = "https://auth.meta.com/oidc/device/authorization/"
	DefaultTokenURL      = "https://auth.meta.com/oidc/device/token/"
	DefaultKeyMintURL    = "https://api.meta.ai/muse-code/key"
	APIVersion           = "1.0.0"
)

type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type PollTokenResult struct {
	Pending          bool
	AccessToken      string
	Error            string
	ErrorDescription string
}

type KeyMintResponse struct {
	APIKey       string `json:"api_key"`
	UserEmail    string `json:"user_email"`
	SubsTierName string `json:"subs_tier_name"`
	IsSubsActive bool   `json:"is_subs_active"`
	ActionURL    string `json:"action_url,omitempty"`
}

type OAuthClient struct {
	httpClient    *http.Client
	clientID      string
	deviceAuthURL string
	tokenURL      string
	keyMintURL    string
	retryBackoff  time.Duration
}

type Option func(*OAuthClient)

func WithDeviceAuthURL(u string) Option {
	return func(c *OAuthClient) { c.deviceAuthURL = u }
}

func WithTokenURL(u string) Option {
	return func(c *OAuthClient) { c.tokenURL = u }
}

func WithKeyMintURL(u string) Option {
	return func(c *OAuthClient) { c.keyMintURL = u }
}

func WithHTTPClient(client *http.Client) Option {
	return func(c *OAuthClient) { c.httpClient = client }
}

func WithRetryBackoff(d time.Duration) Option {
	return func(c *OAuthClient) { c.retryBackoff = d }
}

func NewOAuthClient(opts ...Option) *OAuthClient {
	c := &OAuthClient{
		httpClient:    &http.Client{Timeout: 15 * time.Second},
		clientID:      ClientID,
		deviceAuthURL: DefaultDeviceAuthURL,
		tokenURL:      DefaultTokenURL,
		keyMintURL:    DefaultKeyMintURL,
		retryBackoff:  5 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func IsSafeVerificationURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "auth.meta.com" || strings.HasSuffix(h, ".meta.com") || h == "meta.com"
}

func (c *OAuthClient) RequestDeviceCode(ctx context.Context) (*DeviceCodeResponse, error) {
	data := url.Values{}
	data.Set("client_id", c.clientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.deviceAuthURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create device request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-version", APIVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device authorization failed (status %d)", resp.StatusCode)
	}

	var out DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode device authorization: %w", err)
	}
	if strings.TrimSpace(out.DeviceCode) == "" || strings.TrimSpace(out.UserCode) == "" {
		return nil, errors.New("upstream device response missing required codes")
	}
	if !IsSafeVerificationURL(out.VerificationURI) {
		return nil, fmt.Errorf("upstream returned untrusted verification URL: %s", out.VerificationURI)
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 300
	}
	return &out, nil
}

func (c *OAuthClient) PollToken(ctx context.Context, deviceCode string) (PollTokenResult, error) {
	data := url.Values{}
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	data.Set("device_code", deviceCode)
	data.Set("client_id", c.clientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return PollTokenResult{}, fmt.Errorf("create poll request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-version", APIVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return PollTokenResult{}, fmt.Errorf("poll token: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return PollTokenResult{}, fmt.Errorf("read response: %w", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return PollTokenResult{}, fmt.Errorf("invalid json from token endpoint: %w", err)
	}

	errCode, _ := parsed["error"].(string)

	if errCode == "authorization_pending" || errCode == "slow_down" {
		return PollTokenResult{
			Pending: true,
			Error:   errCode,
		}, nil
	}

	if resp.StatusCode != http.StatusOK || errCode != "" {
		msg := errCode
		if msg == "" {
			msg = fmt.Sprintf("status %d", resp.StatusCode)
		}
		return PollTokenResult{
			Error: errCode,
		}, fmt.Errorf("token endpoint returned %s", msg)
	}

	tok, _ := parsed["access_token"].(string)
	if tok == "" {
		return PollTokenResult{}, errors.New("access_token missing in response")
	}

	return PollTokenResult{
		AccessToken: tok,
	}, nil
}

func (c *OAuthClient) sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *OAuthClient) MintSubscriptionKey(ctx context.Context, accessToken string) (*KeyMintResponse, error) {
	reqBody := []byte(`{"onboard":true}`)
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.keyMintURL, bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("create key mint request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("x-api-version", APIVersion)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if sleepErr := c.sleepWithContext(ctx, c.retryBackoff*time.Duration(attempt+1)); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}

		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read mint response: %w", readErr)
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("mint endpoint returned transient status %d", resp.StatusCode)
			if attempt < 2 {
				if sleepErr := c.sleepWithContext(ctx, c.retryBackoff*time.Duration(attempt+1)); sleepErr != nil {
					return nil, sleepErr
				}
				continue
			}
			return nil, lastErr
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("key mint failed (status %d)", resp.StatusCode)
		}

		var mintResp KeyMintResponse
		if err := json.Unmarshal(b, &mintResp); err != nil {
			return nil, fmt.Errorf("decode key mint response: %w", err)
		}

		if !mintResp.IsSubsActive {
			return nil, errors.New("Muse Code subscription is inactive — please activate it on muse.ai")
		}
		if mintResp.APIKey == "" {
			return nil, errors.New("Muse Code key response missing api_key")
		}

		return &mintResp, nil
	}

	return nil, fmt.Errorf("failed after retries: %w", lastErr)
}
