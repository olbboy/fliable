package rest

import (
	"errors"
	"io"
	"net/http"

	"github.com/olbboy/fliable/form"
	"github.com/olbboy/fliable/store"
)

// WithForms exposes form deployment/lookup endpoints and is normally
// paired with engine.WithFormValidator(reg) so completions validate.
func WithForms(r *form.Registry) Option {
	return func(s *Server) { s.forms = r }
}

func (s *Server) registerFormRoutes() {
	if s.forms == nil {
		return
	}
	s.route("POST /v1/forms", RoleAdmin, s.handleDeployForm)
	s.route("GET /v1/forms", RoleViewer, s.handleListForms)
	s.route("GET /v1/forms/{key}", RoleViewer, s.handleGetForm)
	s.route("DELETE /v1/forms/{key}", RoleAdmin, s.handleDeleteForm)
	s.route("GET /v1/tasks/{id}/form", RoleViewer, s.handleTaskForm)
}

func (s *Server) handleDeployForm(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	doc, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	def, err := s.forms.Deploy(doc)
	if err != nil {
		s.error(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.json(w, http.StatusCreated, def)
}

func (s *Server) handleListForms(w http.ResponseWriter, r *http.Request) {
	defs, err := s.forms.List()
	if err != nil {
		s.fail(w, err)
		return
	}
	if defs == nil {
		defs = []*form.Definition{}
	}
	s.json(w, http.StatusOK, defs)
}

func (s *Server) handleGetForm(w http.ResponseWriter, r *http.Request) {
	def, err := s.forms.Get(r.PathValue("key"))
	if errors.Is(err, form.ErrNotFound) {
		s.error(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, def)
}

func (s *Server) handleDeleteForm(w http.ResponseWriter, r *http.Request) {
	if err := s.forms.Delete(r.PathValue("key")); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusNoContent, nil)
}

// handleTaskForm resolves the form bound to a task — the one call a task
// UI needs to render inputs.
func (s *Server) handleTaskForm(w http.ResponseWriter, r *http.Request) {
	t, err := s.e.Store().GetTask(r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if t.FormKey == "" {
		s.error(w, http.StatusNotFound, errors.New("task has no form"))
		return
	}
	def, err := s.forms.Get(t.FormKey)
	if errors.Is(err, form.ErrNotFound) {
		s.error(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	// Include current instance variables so the client can prefill.
	inst, err := s.e.GetInstance(t.InstanceID)
	vars := map[string]any{}
	if err == nil {
		vars = inst.Variables
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"form": def, "variables": vars})
}
