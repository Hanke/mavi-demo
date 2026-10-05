package server

import (
	"net/http"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/auth"
	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/tasks"
)

// runRoleFilters runs the role's hard filters over the active candidates
// and answers with the recorded run: who passed, how many were left after
// each filter, and the shortlist retrieved from those who passed.
func (s *Server) runRoleFilters(w http.ResponseWriter, r *http.Request) {
	run, err := tasks.HardFilter(r.Context(), s.store, s.tax, r.PathValue("id"), time.Now(), s.retrievalSize)
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

// getRoleMatchStatus answers where the role's matching stands. Ops gets the
// whole of it. An employer is told ready or in_review and nothing of the
// run: a role that needs attention is, to them, still in review.
func (s *Server) getRoleMatchStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.store.RoleMatchStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if identity(r).Role != auth.Ops {
		status.Run = nil
		if status.Status != contract.RoleMatchStateReady {
			status.Status = contract.RoleMatchStateInReview
		}
	}
	writeJSON(w, http.StatusOK, status)
}
