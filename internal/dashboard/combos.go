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

	// A smart combo routes through a System One model and owns no target
	// chain: it carries routes instead, each with its own criteria and
	// destination.
	if strategy == config.StrategySmart {
		s.createSmartCombo(w, r, name)
		return
	}

	newCombo := config.Combo{
		Name:     name,
		Strategy: strategy,
		Targets:  parseComboTargets(r),
		DrainTTL: r.FormValue("drain_ttl"),
		Timeout:  r.FormValue("timeout"),
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

// createSmartCombo stores a smart combo with its routes, default route, and
// decision settings. The combo is validated as a whole, so a default route
// that names no route, or a destination that resolves to nothing, is rejected
// before anything is written.
func (s *Server) createSmartCombo(w http.ResponseWriter, r *http.Request, name string) {
	routes := config.ParseSmartRoutes(r.FormValue("routes"))
	defaultRoute := strings.TrimSpace(r.FormValue("default_route"))
	decision := config.SmartDecision{
		Model:         strings.TrimSpace(r.FormValue("decision_model")),
		Mode:          strings.TrimSpace(r.FormValue("decision_mode")),
		Question:      strings.TrimSpace(r.FormValue("decision_question")),
		MinConfidence: strings.TrimSpace(r.FormValue("decision_min_confidence")),
		Timeout:       strings.TrimSpace(r.FormValue("decision_timeout")),
		BaseURL:       strings.TrimSpace(r.FormValue("decision_base_url")),
		Weights:       config.ParseSmartWeights(r.FormValue("decision_weights")),
	}

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			for _, cb := range c.Combos {
				if cb.Name == name {
					return fmt.Errorf("combo %q already exists", name)
				}
			}
			combo := config.Combo{
				Name:         name,
				Strategy:     config.StrategySmart,
				Routes:       routes,
				DefaultRoute: defaultRoute,
				Decision:     decision,
			}
			// Validate the candidate against the live config before it is
			// stored, so an unresolvable destination never lands on disk.
			candidate := *c
			candidate.Combos = append(append([]config.Combo{}, c.Combos...), combo)
			if err := candidate.Validate(); err != nil {
				return err
			}
			c.Combos = append(c.Combos, combo)
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

// updateCombo replaces a combo's strategy, target chain, and budgets wholesale.
// Any cooldown recorded for a target in the new chain is cleared so the edited
// combo starts from a clean slate. Empty timeout and drain_ttl clear those
// settings; Validate rejects a duration that does not parse.
func (s *Server) updateCombo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	strategy := r.FormValue("strategy")
	if strategy == "" {
		http.Error(w, "strategy is required", http.StatusBadRequest)
		return
	}
	// A smart combo is edited through its routes, not through a target chain.
	if strategy == config.StrategySmart {
		s.updateSmartCombo(w, r, name)
		return
	}
	targets := parseComboTargets(r)
	timeout := strings.TrimSpace(r.FormValue("timeout"))
	drainTTL := strings.TrimSpace(r.FormValue("drain_ttl"))

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			var prev config.Combo
			found := false
			for _, cb := range c.Combos {
				if cb.Name == name {
					prev = cb
					found = true
					break
				}
			}
			err := config.SetCombo(c, name, strategy, targets, timeout, drainTTL)
			if err != nil {
				return err
			}
			if err := c.Validate(); err != nil {
				if found {
					for i := range c.Combos {
						if c.Combos[i].Name == name {
							c.Combos[i] = prev
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

// updateSmartCombo replaces the routes, default route, and decision settings
// of an existing smart combo. A combo that is not a smart one is rejected: its
// chain belongs to the plain editor.
func (s *Server) updateSmartCombo(w http.ResponseWriter, r *http.Request, name string) {
	routes := config.ParseSmartRoutes(r.FormValue("routes"))
	defaultRoute := strings.TrimSpace(r.FormValue("default_route"))
	decision := config.SmartDecision{
		Model:         strings.TrimSpace(r.FormValue("decision_model")),
		Mode:          strings.TrimSpace(r.FormValue("decision_mode")),
		Question:      strings.TrimSpace(r.FormValue("decision_question")),
		MinConfidence: strings.TrimSpace(r.FormValue("decision_min_confidence")),
		Timeout:       strings.TrimSpace(r.FormValue("decision_timeout")),
		BaseURL:       strings.TrimSpace(r.FormValue("decision_base_url")),
		Weights:       config.ParseSmartWeights(r.FormValue("decision_weights")),
	}

	if s.mutate != nil {
		err := s.mutate(func(c *config.Config) error {
			existing, ok := config.FindCombo(c, name)
			if !ok {
				return fmt.Errorf("combo %q not found", name)
			}
			if !existing.IsSmart() {
				return fmt.Errorf("combo %q is not a smart combo", name)
			}
			if err := config.SetSmartCombo(c, name, routes, defaultRoute, decision); err != nil {
				return err
			}
			return c.Validate()
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

// comboAction dispatches a /combos/{name...} request to the edit or the
// delete handler.
//
// The route carries the whole rest of the path because a combo name may
// contain slashes. The trailing "/delete" is the action only when no combo
// by that exact name exists: a combo literally named "combo/delete" must
// still be editable, so the name lookup decides, not the suffix.
func (s *Server) comboAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "combo name is required", http.StatusBadRequest)
		return
	}
	if _, ok := config.FindCombo(s.getCfg(), name); ok {
		s.updateCombo(w, r)
		return
	}
	if trimmed, ok := strings.CutSuffix(name, "/delete"); ok {
		if _, exists := config.FindCombo(s.getCfg(), trimmed); exists {
			r.SetPathValue("name", trimmed)
			s.deleteCombo(w, r)
			return
		}
	}
	// Not an existing combo and not a delete of one: treat the whole path as
	// a name so an unknown combo produces the normal validation error.
	r.SetPathValue("name", name)
	s.updateCombo(w, r)
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

// getComboPickerModels re-renders the picker's provider/model list from the
// current config so a model catalog refreshed after page load is visible
// without a full page reload.
func (s *Server) getComboPickerModels(w http.ResponseWriter, r *http.Request) {
	_ = s.tmpl.ExecuteTemplate(w, "combo-picker-models", viewData{Providers: s.getCfg().Providers})
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
	cfg := s.getCfg()
	data := viewData{
		Providers: cfg.Providers,
		Combos:    cfg.Combos,
		Tracker:   s.tracker,
	}
	// The smart router's destination picker offers the plain combos a route
	// can point at, plus every provider/model pair it can name directly. A
	// smart combo is left out: a route may only target a plain combo.
	for _, cb := range cfg.Combos {
		if !cb.IsSmart() {
			data.SmartComboNames = append(data.SmartComboNames, cb.Name)
		}
	}
	for _, p := range cfg.Providers {
		if p.Disabled {
			continue
		}
		for _, m := range p.Models {
			if p.IsModelDisabled(m) {
				continue
			}
			data.SmartModelTargets = append(data.SmartModelTargets, p.Name+"/"+m)
		}
	}
	_ = s.tmpl.ExecuteTemplate(w, "combos", data)
}
