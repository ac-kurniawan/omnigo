package dashboard

import (
	"fmt"
	"log"
	"net/http"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func (s *Server) createProvider(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	typ := r.FormValue("type")
	baseURL := r.FormValue("base_url")
	apiKey := r.FormValue("api_key")

	if name == "" || typ == "" {
		http.Error(w, "name and type are required", http.StatusBadRequest)
		return
	}

	newP := config.Provider{
		Name:    name,
		Type:    typ,
		BaseURL: baseURL,
	}

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			for _, p := range c.Providers {
				if p.Name == name {
					return fmt.Errorf("provider %q already exists", name)
				}
			}
			c.Providers = append(c.Providers, newP)
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if apiKey != "" {
		if err := s.store.Update(func(v *vault.Vault) error {
			accounts := v.Accounts(name)
			if len(accounts) == 0 {
				v.UpsertAccount(name, vault.ProviderSecret{APIKey: apiKey})
			} else {
				accounts[0].APIKey = apiKey
				if v.ProviderAccounts == nil {
					v.ProviderAccounts = make(map[string][]vault.ProviderSecret)
				}
				v.ProviderAccounts[name] = accounts
				delete(v.ProviderSecrets, name)
			}
			return nil
		}); err != nil {
			http.Error(w, "failed to save provider key", http.StatusInternalServerError)
			return
		}
	}

	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderProviders(w)
}

func (s *Server) setProviderKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	apiKey := r.FormValue("api_key")

	if err := s.store.Update(func(v *vault.Vault) error {
		accounts := v.Accounts(name)
		if len(accounts) == 0 {
			v.UpsertAccount(name, vault.ProviderSecret{APIKey: apiKey})
		} else {
			accounts[0].APIKey = apiKey
			if v.ProviderAccounts == nil {
				v.ProviderAccounts = make(map[string][]vault.ProviderSecret)
			}
			v.ProviderAccounts[name] = accounts
			delete(v.ProviderSecrets, name)
		}
		return nil
	}); err != nil {
		http.Error(w, "failed to save provider key", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderProviders(w)
}

func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			var kept []config.Provider
			for _, p := range c.Providers {
				if p.Name != name {
					kept = append(kept, p)
				}
			}
			c.Providers = kept
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	_ = s.store.Update(func(v *vault.Vault) error {
		delete(v.ProviderSecrets, name)
		delete(v.ProviderAccounts, name)
		return nil
	})

	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderProviders(w)
}

func (s *Server) deleteProviderAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	identity := r.PathValue("identity")
	if s.findProvider(name) == nil || identity == "" {
		http.NotFound(w, r)
		return
	}
	if err := s.store.Update(func(v *vault.Vault) error {
		accounts := v.Accounts(name)
		kept := make([]vault.ProviderSecret, 0, len(accounts))
		for _, account := range accounts {
			if account.Identity() != identity {
				kept = append(kept, account)
			}
		}
		if len(kept) == len(accounts) {
			return fmt.Errorf("account not found")
		}
		if v.ProviderAccounts == nil {
			v.ProviderAccounts = make(map[string][]vault.ProviderSecret)
		}
		v.ProviderAccounts[name] = kept
		delete(v.ProviderSecrets, name)
		return nil
	}); err != nil {
		http.Error(w, "failed to remove account", http.StatusBadRequest)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderProviders(w)
}

func (s *Server) toggleProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			for _, p := range c.Providers {
				if p.Name == name {
					return config.SetProviderDisabled(c, name, !p.Disabled)
				}
			}
			return fmt.Errorf("provider %q not found", name)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderProviders(w)
}

func (s *Server) getProviders(w http.ResponseWriter, r *http.Request) {
	s.renderProviders(w)
}

func (s *Server) renderProviders(w http.ResponseWriter) {
	snapshot := s.store.Get()
	data := viewData{
		Providers: s.getCfg().Providers,
		Secrets:   snapshot.ProviderSecrets,
		Accounts:  snapshot.ProviderAccounts,
		Tracker:   s.tracker,
		Quota:     s.quotaCache,
	}
	if err := s.tmpl.ExecuteTemplate(w, "providers", data); err != nil {
		// A template error after headers are sent cannot change the status, so
		// log it: silently truncating the page hides the failure from the
		// operator entirely.
		log.Printf("dashboard: render providers: %v", err)
	}
}
