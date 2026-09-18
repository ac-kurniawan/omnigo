package antigravity

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// TokenManager owns refresh for one credential pair: it serializes refreshes,
// refreshes eagerly when the access token is expiring, and remembers a token
// pair the endpoint rejected unrecoverably so a dead credential cannot be
// re-POSTed on every request.
type TokenManager struct {
	store provider.CredStore

	mu               sync.Mutex
	deadAccessToken  string
	deadRefreshToken string
}

func NewTokenManager(store provider.CredStore) *TokenManager {
	return &TokenManager{store: store}
}

// EnsureFreshToken returns credentials usable for the next request, refreshing
// when the access token is missing or expires within 5 minutes.
func (m *TokenManager) EnsureFreshToken(ctx context.Context) (provider.Credentials, error) {
	c := m.store.Get()
	if c.AccessToken == "" && c.RefreshToken == "" {
		return c, fmt.Errorf("antigravity: not authenticated")
	}
	if m.credentialsDead(c) {
		return c, reauthenticationError()
	}
	if tokenFresh(c) {
		return c, nil
	}
	if c.RefreshToken == "" {
		return c, fmt.Errorf("antigravity: token expired and no refresh token")
	}
	return m.refresh(ctx, "", false)
}

// ForceRefreshToken refreshes after an upstream rejection. previousAccessToken
// is the token that was rejected: when the store already carries a different
// one, a concurrent request refreshed first and its result is reused.
func (m *TokenManager) ForceRefreshToken(ctx context.Context, previousAccessToken string) (provider.Credentials, error) {
	return m.refresh(ctx, previousAccessToken, true)
}

func (m *TokenManager) refresh(ctx context.Context, previousAccessToken string, forced bool) (provider.Credentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.store.Get()
	if forced {
		if previousAccessToken != "" && c.AccessToken != previousAccessToken {
			return c, nil
		}
	} else if tokenFresh(c) {
		return c, nil
	}
	if c.RefreshToken == "" {
		return c, fmt.Errorf("antigravity: no refresh token")
	}
	if c.AccessToken == m.deadAccessToken && c.RefreshToken == m.deadRefreshToken {
		return c, reauthenticationError()
	}
	m.deadAccessToken = ""
	m.deadRefreshToken = ""

	tok, err := Refresh(ctx, c.RefreshToken)
	if err != nil {
		if isUnrecoverableRefreshError(err) {
			m.deadAccessToken = c.AccessToken
			m.deadRefreshToken = c.RefreshToken
			return c, reauthenticationError()
		}
		return c, err
	}
	c.AccessToken = tok.AccessToken
	c.ExpiresAt = tok.ExpiresAt
	if tok.RefreshToken != "" {
		c.RefreshToken = tok.RefreshToken
	}
	// Discover project if empty
	if c.ProjectID == "" {
		if pid, err := DiscoverProject(ctx, c.AccessToken); err == nil && pid != "" {
			c.ProjectID = pid
		}
	}
	if err := m.store.Put(c); err != nil {
		return c, err
	}
	return c, nil
}

func (m *TokenManager) credentialsDead(c provider.Credentials) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return c.AccessToken == m.deadAccessToken && c.RefreshToken == m.deadRefreshToken
}

func reauthenticationError() error {
	return fmt.Errorf("antigravity: re-authentication required; use Connect Google")
}

func tokenFresh(c provider.Credentials) bool {
	return c.AccessToken != "" && !c.ExpiresAt.IsZero() && time.Until(c.ExpiresAt) > 5*time.Minute
}
