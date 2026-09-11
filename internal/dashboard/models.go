package dashboard

import (
	"net/http"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/config"
)

func (s *Server) addModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	modelID := strings.TrimSpace(r.FormValue("model_id"))
	if modelID == "" {
		http.Error(w, "model_id is required", http.StatusBadRequest)
		return
	}

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			for i := range c.Providers {
				if c.Providers[i].Name == name {
					p := &c.Providers[i]
					// Remove from disabled if present
					var newDisabled []string
					for _, m := range p.DisabledModels {
						if m != modelID {
							newDisabled = append(newDisabled, m)
						}
					}
					p.DisabledModels = newDisabled

					// Add to models if not present
					for _, m := range p.Models {
						if m == modelID {
							return nil
						}
					}
					p.Models = append(p.Models, modelID)
					return nil
				}
			}
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderProviders(w)
}

func (s *Server) disableModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	modelID := strings.TrimSpace(r.FormValue("model_id"))
	if modelID == "" {
		http.Error(w, "model_id is required", http.StatusBadRequest)
		return
	}

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			for i := range c.Providers {
				if c.Providers[i].Name == name {
					p := &c.Providers[i]
					var newActive []string
					for _, m := range p.Models {
						if m != modelID {
							newActive = append(newActive, m)
						}
					}
					p.Models = newActive

					for _, m := range p.DisabledModels {
						if m == modelID {
							return nil
						}
					}
					p.DisabledModels = append(p.DisabledModels, modelID)
					return nil
				}
			}
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderProviders(w)
}

func (s *Server) enableModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	modelID := strings.TrimSpace(r.FormValue("model_id"))
	if modelID == "" {
		http.Error(w, "model_id is required", http.StatusBadRequest)
		return
	}

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			for i := range c.Providers {
				if c.Providers[i].Name == name {
					p := &c.Providers[i]
					var newDisabled []string
					for _, m := range p.DisabledModels {
						if m != modelID {
							newDisabled = append(newDisabled, m)
						}
					}
					p.DisabledModels = newDisabled

					for _, m := range p.Models {
						if m == modelID {
							return nil
						}
					}
					p.Models = append(p.Models, modelID)
					return nil
				}
			}
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderProviders(w)
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	modelID := strings.TrimSpace(r.FormValue("model_id"))
	if modelID == "" {
		http.Error(w, "model_id is required", http.StatusBadRequest)
		return
	}

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			for i := range c.Providers {
				if c.Providers[i].Name == name {
					p := &c.Providers[i]
					var newActive []string
					for _, m := range p.Models {
						if m != modelID {
							newActive = append(newActive, m)
						}
					}
					p.Models = newActive

					var newDisabled []string
					for _, m := range p.DisabledModels {
						if m != modelID {
							newDisabled = append(newDisabled, m)
						}
					}
					p.DisabledModels = newDisabled
					return nil
				}
			}
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderProviders(w)
}
