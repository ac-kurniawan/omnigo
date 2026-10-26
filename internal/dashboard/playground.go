package dashboard

import "net/http"

// getPlaygroundModels re-renders the playground's model/combo options from the
// current config, so a provider or combo added from another tab is selectable
// without a full page reload.
func (s *Server) getPlaygroundModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.getCfg()
	_ = s.tmpl.ExecuteTemplate(w, "playground-model-options", viewData{
		Providers: cfg.Providers,
		Combos:    cfg.Combos,
	})
}
