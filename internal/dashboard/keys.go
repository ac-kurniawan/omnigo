package dashboard

import (
	"net/http"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	raw, hash, prefix, err := auth.GenerateKey()
	if err != nil {
		http.Error(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	key := vault.ClientKey{
		ID:        randomHex(8),
		Name:      name,
		KeyHash:   hash,
		Prefix:    prefix,
		CreatedAt: time.Now(),
		Active:    true,
	}
	if err := s.store.Update(func(v *vault.Vault) error {
		v.ClientKeys = append(v.ClientKeys, key)
		return nil
	}); err != nil {
		http.Error(w, "key persistence failed", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderKeys(w, raw)
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.Update(func(v *vault.Vault) error {
		for i := range v.ClientKeys {
			if v.ClientKeys[i].ID == id {
				v.ClientKeys[i].Active = false
			}
		}
		return nil
	}); err != nil {
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderKeys(w, "")
}

func (s *Server) getKeys(w http.ResponseWriter, r *http.Request) {
	s.renderKeys(w, "")
}

func (s *Server) renderKeys(w http.ResponseWriter, newKey string) {
	data := viewData{Keys: s.store.Get().ClientKeys, NewKey: newKey}
	_ = s.tmpl.ExecuteTemplate(w, "keys", data)
}
