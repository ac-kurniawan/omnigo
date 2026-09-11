package dashboard

import (
	"fmt"
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
			if v.ProviderSecrets == nil {
				v.ProviderSecrets = make(map[string]vault.ProviderSecret)
			}
			sec := v.ProviderSecrets[name]
			sec.APIKey = apiKey
			v.ProviderSecrets[name] = sec
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
		if v.ProviderSecrets == nil {
			v.ProviderSecrets = make(map[string]vault.ProviderSecret)
		}
		sec := v.ProviderSecrets[name]
		sec.APIKey = apiKey
		v.ProviderSecrets[name] = sec
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
		return nil
	})

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
	data := viewData{
		Providers: s.getCfg().Providers,
		Secrets:   s.store.Get().ProviderSecrets,
	}
	_ = s.tmpl.ExecuteTemplate(w, "providers", data)
}
