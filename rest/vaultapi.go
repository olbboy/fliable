package rest

import (
	"errors"
	"net/http"

	"github.com/olbboy/fliable/vault"
)

// WithVault exposes the secrets API (admin-only). Values are write-mostly:
// listing returns names only; reading a value back requires the explicit
// /value endpoint so accidental exposure in dashboards is structurally
// awkward.
func WithVault(v *vault.Vault) Option {
	return func(s *Server) { s.vault = v }
}

func (s *Server) registerVaultRoutes() {
	if s.vault == nil {
		return
	}
	s.route("GET /v1/secrets", RoleAdmin, s.handleListSecrets)
	s.route("PUT /v1/secrets/{name}", RoleAdmin, s.handlePutSecret)
	s.route("GET /v1/secrets/{name}/value", RoleAdmin, s.handleGetSecretValue)
	s.route("DELETE /v1/secrets/{name}", RoleAdmin, s.handleDeleteSecret)
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	names, err := s.vault.List()
	if err != nil {
		s.fail(w, err)
		return
	}
	if names == nil {
		names = []string{}
	}
	s.json(w, http.StatusOK, map[string]any{"names": names})
}

func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Value string `json:"value"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	if err := s.vault.Set(r.PathValue("name"), req.Value); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusNoContent, nil)
}

func (s *Server) handleGetSecretValue(w http.ResponseWriter, r *http.Request) {
	val, err := s.vault.Get(r.PathValue("name"))
	if errors.Is(err, vault.ErrNotFound) {
		s.error(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"value": val})
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	if err := s.vault.Delete(r.PathValue("name")); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusNoContent, nil)
}
