package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

type credStore struct {
	store *vault.Store
	name  string
}

func (s credStore) Get() provider.Credentials {
	v := s.store.Get()
	sec := v.ProviderSecrets[s.name]
	return provider.Credentials{
		APIKey:       sec.APIKey,
		AccessToken:  sec.AccessToken,
		RefreshToken: sec.RefreshToken,
		IDToken:      sec.IDToken,
		ExpiresAt:    sec.ExpiresAt,
		ProjectID:    sec.ProjectID,
		AccountID:    sec.AccountID,
		Email:        sec.Email,
	}
}

func (s credStore) Put(c provider.Credentials) error {
	return s.store.Update(func(v *vault.Vault) error {
		if v.ProviderSecrets == nil {
			v.ProviderSecrets = make(map[string]vault.ProviderSecret)
		}
		sec := v.ProviderSecrets[s.name]
		sec.APIKey = c.APIKey
		sec.AccessToken = c.AccessToken
		sec.RefreshToken = c.RefreshToken
		sec.IDToken = c.IDToken
		sec.ExpiresAt = c.ExpiresAt
		sec.ProjectID = c.ProjectID
		sec.AccountID = c.AccountID
		sec.Email = c.Email
		v.ProviderSecrets[s.name] = sec
		return nil
	})
}

func buildProvider(cfg config.Provider, store *vault.Store, transport http.RoundTripper, defaultTimeout ...time.Duration) (provider.Provider, error) {
	factory, ok := provider.Get(cfg.Type)
	if !ok {
		return nil, fmt.Errorf("unknown provider type %q", cfg.Type)
	}
	def := 30 * time.Second
	if len(defaultTimeout) > 0 && defaultTimeout[0] > 0 {
		def = defaultTimeout[0]
	}
	timeout := cfg.ParsedTimeout(def)
	return factory(provider.Config{
		Name:      cfg.Name,
		BaseURL:   cfg.BaseURL,
		Models:    cfg.Models,
		Timeout:   timeout,
		Transport: transport,
	}, credStore{store: store, name: cfg.Name}), nil
}
