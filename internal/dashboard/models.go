package dashboard

import (
	"net/http"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/config"
)

func (s *Server) renderModelModal(w http.ResponseWriter, name string) {
	for _, p := range s.getCfg().Providers {
		if p.Name == name {
			_ = s.tmpl.ExecuteTemplate(w, "models-modal-content", p)
			return
		}
	}
	s.renderProviders(w)
}

func parseModelIDs(r *http.Request) []string {
	_ = r.ParseForm()
	ids := r.Form["model_id"]
	if len(ids) == 0 {
		if val := strings.TrimSpace(r.FormValue("model_id")); val != "" {
			ids = []string{val}
		}
	}
	var res []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			res = append(res, id)
		}
	}
	return res
}

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
	s.renderModelModal(w, name)
}

func (s *Server) disableModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	modelIDs := parseModelIDs(r)
	if len(modelIDs) == 0 {
		http.Error(w, "model_id is required", http.StatusBadRequest)
		return
	}

	disableSet := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		disableSet[id] = true
	}

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			for i := range c.Providers {
				if c.Providers[i].Name == name {
					p := &c.Providers[i]
					var newActive []string
					for _, m := range p.Models {
						if !disableSet[m] {
							newActive = append(newActive, m)
						}
					}
					p.Models = newActive

					existingDisabled := make(map[string]bool, len(p.DisabledModels))
					for _, m := range p.DisabledModels {
						existingDisabled[m] = true
					}
					for id := range disableSet {
						if !existingDisabled[id] {
							p.DisabledModels = append(p.DisabledModels, id)
							existingDisabled[id] = true
						}
					}
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
	s.renderModelModal(w, name)
}

func (s *Server) enableModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	modelIDs := parseModelIDs(r)
	if len(modelIDs) == 0 {
		http.Error(w, "model_id is required", http.StatusBadRequest)
		return
	}

	enableSet := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		enableSet[id] = true
	}

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			for i := range c.Providers {
				if c.Providers[i].Name == name {
					p := &c.Providers[i]
					var newDisabled []string
					for _, m := range p.DisabledModels {
						if !enableSet[m] {
							newDisabled = append(newDisabled, m)
						}
					}
					p.DisabledModels = newDisabled

					existingActive := make(map[string]bool, len(p.Models))
					for _, m := range p.Models {
						existingActive[m] = true
					}
					for id := range enableSet {
						if !existingActive[id] {
							p.Models = append(p.Models, id)
							existingActive[id] = true
						}
					}
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
	s.renderModelModal(w, name)
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	modelIDs := parseModelIDs(r)
	if len(modelIDs) == 0 {
		http.Error(w, "model_id is required", http.StatusBadRequest)
		return
	}

	deleteSet := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		deleteSet[id] = true
	}

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			for i := range c.Providers {
				if c.Providers[i].Name == name {
					p := &c.Providers[i]
					var newActive []string
					for _, m := range p.Models {
						if !deleteSet[m] {
							newActive = append(newActive, m)
						}
					}
					p.Models = newActive

					var newDisabled []string
					for _, m := range p.DisabledModels {
						if !deleteSet[m] {
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
	s.renderModelModal(w, name)
}
