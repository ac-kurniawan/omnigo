package dashboard

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/config"
)

func (s *Server) createCombo(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	strategy := r.FormValue("strategy")
	targetsStr := r.FormValue("targets")

	if name == "" || strategy == "" {
		http.Error(w, "name and strategy are required", http.StatusBadRequest)
		return
	}

	var targets []config.ComboTarget
	rawTargets := strings.FieldsFunc(targetsStr, func(c rune) bool {
		return c == ',' || c == '\n' || c == '\r' || c == ' '
	})
	for _, t := range rawTargets {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		parts := strings.SplitN(t, "/", 2)
		if len(parts) == 2 {
			targets = append(targets, config.ComboTarget{
				Provider: parts[0],
				Model:    parts[1],
			})
		}
	}

	newCombo := config.Combo{
		Name:     name,
		Strategy: strategy,
		Targets:  targets,
	}

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			for _, cb := range c.Combos {
				if cb.Name == name {
					return fmt.Errorf("combo %q already exists", name)
				}
			}
			c.Combos = append(c.Combos, newCombo)
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
	s.renderCombos(w)
}

func (s *Server) deleteCombo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			var kept []config.Combo
			for _, cb := range c.Combos {
				if cb.Name != name {
					kept = append(kept, cb)
				}
			}
			c.Combos = kept
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
	s.renderCombos(w)
}

func (s *Server) getCombos(w http.ResponseWriter, r *http.Request) {
	s.renderCombos(w)
}

func (s *Server) renderCombos(w http.ResponseWriter) {
	data := viewData{Combos: s.getCfg().Combos}
	_ = s.tmpl.ExecuteTemplate(w, "combos", data)
}
