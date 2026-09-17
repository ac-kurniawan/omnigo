package dashboard

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/ac-kurniawan/omnigo/internal/combo"
	"github.com/ac-kurniawan/omnigo/internal/config"
)

func (s *Server) createCombo(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	strategy := r.FormValue("strategy")
	_ = r.ParseForm()

	if name == "" || strategy == "" {
		http.Error(w, "name and strategy are required", http.StatusBadRequest)
		return
	}

	newCombo := config.Combo{
		Name:     name,
		Strategy: strategy,
		Targets:  parseComboTargets(r),
		DrainTTL: r.FormValue("drain_ttl"),
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

// parseComboTargets reads the ordered target chain from the request form,
// accepting either repeated "targets" values or comma/space/newline-delimited
// strings of "<provider>/<model>" entries.
func parseComboTargets(r *http.Request) []config.ComboTarget {
	_ = r.ParseForm()
	var targets []config.ComboTarget
	for _, raw := range r.Form["targets"] {
		for _, item := range strings.FieldsFunc(raw, func(c rune) bool {
			return c == ',' || c == '\n' || c == '\r' || c == ' '
		}) {
			item = strings.TrimSpace(item)
			provider, model, ok := strings.Cut(item, "/")
			if !ok || provider == "" || model == "" {
				continue
			}
			targets = append(targets, config.ComboTarget{Provider: provider, Model: model})
		}
	}
	return targets
}

// updateCombo replaces a combo's strategy and target chain wholesale. Any
// cooldown recorded for a target in the new chain is cleared so the edited
// combo starts from a clean slate.
func (s *Server) updateCombo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	strategy := r.FormValue("strategy")
	if strategy == "" {
		http.Error(w, "strategy is required", http.StatusBadRequest)
		return
	}
	targets := parseComboTargets(r)

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			return config.SetCombo(c, name, strategy, targets)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if s.tracker != nil {
		for _, t := range targets {
			target := combo.Target{Provider: t.Provider, Model: t.Model}
			if s.tracker.IsDrained(target) {
				s.tracker.Clear(target)
			}
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

func (s *Server) resetDrain(w http.ResponseWriter, r *http.Request) {
	provider := r.FormValue("provider")
	model := r.FormValue("model")
	if s.tracker != nil && provider != "" && model != "" {
		s.tracker.Clear(combo.Target{Provider: provider, Model: model})
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderCombos(w)
}

func (s *Server) resetAllDrains(w http.ResponseWriter, r *http.Request) {
	if s.tracker != nil {
		s.tracker.ClearAll()
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderCombos(w)
}

func (s *Server) renderCombos(w http.ResponseWriter) {
	data := viewData{
		Providers: s.getCfg().Providers,
		Combos:    s.getCfg().Combos,
		Tracker:   s.tracker,
	}
	_ = s.tmpl.ExecuteTemplate(w, "combos", data)
}
