package dashboard

import (
	"fmt"
	"log"
	"net/http"
	"strings"

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

// updateProviderTimeouts replaces one provider's request and stream timeout
// overrides. Empty values clear the override so the provider inherits the
// server default. An invalid duration is rejected and the previous values are
// restored, matching the combo edit path.
func (s *Server) updateProviderTimeouts(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	timeout := strings.TrimSpace(r.FormValue("timeout"))
	streamTimeout := strings.TrimSpace(r.FormValue("stream_timeout"))

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			var prev config.Provider
			found := false
			for _, p := range c.Providers {
				if p.Name == name {
					prev = p
					found = true
					break
				}
			}
			if err := config.SetProviderTimeouts(c, name, timeout, streamTimeout); err != nil {
				return err
			}
			if err := c.Validate(); err != nil {
				if found {
					for i := range c.Providers {
						if c.Providers[i].Name == name {
							c.Providers[i] = prev
							break
						}
					}
				}
				return err
			}
			return nil
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

// updateServerTimeouts replaces the server-wide request and stream timeout
// defaults. Empty values restore the built-in defaults (60s and 15m). "0" for
// the stream timeout is accepted and means unbounded.
func (s *Server) updateServerTimeouts(w http.ResponseWriter, r *http.Request) {
	timeout := strings.TrimSpace(r.FormValue("timeout"))
	streamTimeout := strings.TrimSpace(r.FormValue("stream_timeout"))

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			prev := c.Server
			if err := config.SetServerTimeouts(c, timeout, streamTimeout); err != nil {
				return err
			}
			if err := c.Validate(); err != nil {
				c.Server = prev
				return err
			}
			return nil
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
	s.renderSettings(w)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w)
}

func (s *Server) renderSettings(w http.ResponseWriter) {
	data := viewData{Server: s.getCfg().Server}
	if err := s.tmpl.ExecuteTemplate(w, "settings", data); err != nil {
		log.Printf("dashboard: render settings: %v", err)
	}
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
