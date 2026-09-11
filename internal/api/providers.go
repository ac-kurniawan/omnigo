package api

import (
	"fmt"
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
		ExpiresAt:    sec.ExpiresAt,
		ProjectID:    sec.ProjectID,
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
		sec.ExpiresAt = c.ExpiresAt
		sec.ProjectID = c.ProjectID
		v.ProviderSecrets[s.name] = sec
		return nil
	})
}

func buildProvider(cfg config.Provider, store *vault.Store, defaultTimeout ...time.Duration) (provider.Provider, error) {
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
		Name:    cfg.Name,
		BaseURL: cfg.BaseURL,
		Models:  cfg.Models,
		Timeout: timeout,
	}, credStore{store: store, name: cfg.Name}), nil
}
