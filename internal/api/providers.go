package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	_ "github.com/ac-kurniawan/omnigo/internal/provider/antigravity"
	_ "github.com/ac-kurniawan/omnigo/internal/provider/codex"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

type credStore struct {
	store *vault.Store
	name  string
}

func (s credStore) Get() provider.Credentials {
	accounts := s.Accounts()
	if len(accounts) == 0 {
		return provider.Credentials{}
	}
	return accounts[0]
}

func (s credStore) Accounts() []provider.Credentials {
	secrets := s.store.Get().Accounts(s.name)
	accounts := make([]provider.Credentials, 0, len(secrets))
	for _, secret := range secrets {
		accounts = append(accounts, credentialsFromSecret(secret))
	}
	return accounts
}

func (s credStore) Put(c provider.Credentials) error {
	identity := c.Identity()
	if identity == "" {
		current := s.Get()
		identity = current.Identity()
	}
	return s.PutAccount(identity, c)
}

func (s credStore) PutAccount(identity string, c provider.Credentials) error {
	return s.store.Update(func(v *vault.Vault) error {
		accounts := v.Accounts(s.name)
		for i := range accounts {
			if (identity != "" && accounts[i].Identity() == identity) || (identity == "" && i == 0) {
				accounts[i] = secretFromCredentials(c)
				if v.ProviderAccounts == nil {
					v.ProviderAccounts = make(map[string][]vault.ProviderSecret)
				}
				v.ProviderAccounts[s.name] = accounts
				delete(v.ProviderSecrets, s.name)
				return nil
			}
		}
		v.UpsertAccount(s.name, secretFromCredentials(c))
		return nil
	})
}

func credentialsFromSecret(sec vault.ProviderSecret) provider.Credentials {
	return provider.Credentials{
		APIKey: sec.APIKey, AccessToken: sec.AccessToken, RefreshToken: sec.RefreshToken,
		IDToken: sec.IDToken, ExpiresAt: sec.ExpiresAt, ProjectID: sec.ProjectID,
		AccountID: sec.AccountID, Email: sec.Email,
	}
}

func secretFromCredentials(c provider.Credentials) vault.ProviderSecret {
	return vault.ProviderSecret{
		APIKey: c.APIKey, AccessToken: c.AccessToken, RefreshToken: c.RefreshToken,
		IDToken: c.IDToken, ExpiresAt: c.ExpiresAt, ProjectID: c.ProjectID,
		AccountID: c.AccountID, Email: c.Email,
	}
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
	p := factory(provider.Config{
		Name:      cfg.Name,
		BaseURL:   cfg.BaseURL,
		Models:    cfg.Models,
		Timeout:   timeout,
		Transport: transport,
	}, credStore{store: store, name: cfg.Name})
	if cfg.MaxConcurrency > 0 {
		p = provider.WithConcurrencyLimit(p, cfg.MaxConcurrency)
	}
	return p, nil
}
