package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type TokenManager struct {
	store provider.CredStore
	mu    sync.Mutex
}

func NewTokenManager(store provider.CredStore) *TokenManager {
	return &TokenManager{store: store}
}

func (m *TokenManager) EnsureFreshToken(ctx context.Context) (provider.Credentials, error) {
	creds := m.store.Get()
	if creds.AccessToken == "" && creds.RefreshToken == "" {
		return creds, fmt.Errorf("codex: not authenticated")
	}
	if tokenFresh(creds) {
		return creds, nil
	}
	return m.refresh(ctx, "", false)
}

func (m *TokenManager) ForceRefreshToken(ctx context.Context, previousAccessToken string) (provider.Credentials, error) {
	return m.refresh(ctx, previousAccessToken, true)
}

func (m *TokenManager) refresh(ctx context.Context, previousAccessToken string, forced bool) (provider.Credentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	creds := m.store.Get()
	if forced {
		if previousAccessToken != "" && creds.AccessToken != previousAccessToken {
			return creds, nil
		}
	} else if tokenFresh(creds) {
		return creds, nil
	}
	if creds.RefreshToken == "" {
		return creds, fmt.Errorf("codex: no refresh token")
	}

	tok, err := Refresh(ctx, creds.RefreshToken)
	if err != nil {
		return creds, err
	}
	creds.AccessToken = tok.AccessToken
	creds.ExpiresAt = tok.ExpiresAt
	if tok.RefreshToken != "" {
		creds.RefreshToken = tok.RefreshToken
	}
	if tok.IDToken != "" {
		if claims, err := ParseIDTokenClaims(tok.IDToken); err == nil {
			creds.IDToken = tok.IDToken
			creds.Email = claims.Email
			creds.AccountID = claims.AccountID
		}
	}
	if err := m.store.Put(creds); err != nil {
		return creds, fmt.Errorf("persist refreshed Codex credentials: %w", err)
	}
	return creds, nil
}

func tokenFresh(creds provider.Credentials) bool {
	return creds.AccessToken != "" && !creds.ExpiresAt.IsZero() && time.Until(creds.ExpiresAt) > 5*time.Minute
}

func Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	body, err := json.Marshal(map[string]string{
		"client_id":     ClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	if err != nil {
		return nil, fmt.Errorf("encode refresh request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return exchange(req)
}
