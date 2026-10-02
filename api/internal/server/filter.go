package server

import (
	"net/http"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/tasks"
)

// runRoleFilters runs the role's hard filters over the active candidates
// and answers with the recorded run: who passed, and how many were left
// after each filter.
func (s *Server) runRoleFilters(w http.ResponseWriter, r *http.Request) {
	run, err := tasks.HardFilter(r.Context(), s.store, s.tax, r.PathValue("id"), time.Now())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) listRoleFilterRuns(w http.ResponseWriter, r *http.Request) {
	role, err := s.store.GetRole(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	runs, err := s.store.ListFilterRuns(r.Context(), role.ID, pageFrom(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}
