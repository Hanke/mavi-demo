package server

import (
	"net/http"

	"github.com/colehanke/mavi-demo/api/internal/auth"
	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/store"
)

// Human validation: ops decides on a role's matches (approve, reject, swap)
// and releases the role once exactly two are approved. The rules, and the
// review_events row each decision writes, are the store's (store/review.go).

// reviewer reads who is deciding and their optional reason. The audit trail
// is of who did what, so a review action that does not say who is refused;
// it writes the response and returns false when it did.
func reviewer(w http.ResponseWriter, r *http.Request) (store.Reviewer, bool) {
	actor := identity(r).Actor
	if actor == "" {
		writeError(w, http.StatusForbidden, "review actions must set "+auth.ActorHeader+" to the reviewer, who is recorded in the audit trail")
		return store.Reviewer{}, false
	}
	var b contract.ReviewInput
	if _, ok := decodeBody(w, r, &b, true); !ok {
		return store.Reviewer{}, false
	}
	return store.Reviewer{Actor: actor, Reason: strOr(b.Reason, "")}, true
}

func (s *Server) getReviewQueue(w http.ResponseWriter, r *http.Request) {
	queue, err := s.store.ReviewQueue(r.Context(), r.PathValue("id"), s.minScore)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, queue)
}

func (s *Server) approveMatch(w http.ResponseWriter, r *http.Request) {
	by, ok := reviewer(w, r)
	if !ok {
		return
	}
	m, err := s.store.ApproveMatch(r.Context(), r.PathValue("id"), by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) rejectMatch(w http.ResponseWriter, r *http.Request) {
	by, ok := reviewer(w, r)
	if !ok {
		return
	}
	m, err := s.store.RejectMatch(r.Context(), r.PathValue("id"), by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) swapMatch(w http.ResponseWriter, r *http.Request) {
	by, ok := reviewer(w, r)
	if !ok {
		return
	}
	swap, err := s.store.SwapMatch(r.Context(), r.PathValue("id"), s.minScore, by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, swap)
}

// releaseRole is the one way a match reaches the employer: both of the
// role's approved matches at once, and only when there are exactly two.
func (s *Server) releaseRole(w http.ResponseWriter, r *http.Request) {
	by, ok := reviewer(w, r)
	if !ok {
		return
	}
	released, err := s.store.ReleaseRole(r.Context(), r.PathValue("id"), by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, released)
}

func (s *Server) unreleaseMatch(w http.ResponseWriter, r *http.Request) {
	by, ok := reviewer(w, r)
	if !ok {
		return
	}
	m, err := s.store.UnreleaseMatch(r.Context(), r.PathValue("id"), by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) listReviewEvents(w http.ResponseWriter, r *http.Request) {
	role, err := s.store.GetRole(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	events, err := s.store.ListReviewEvents(r.Context(), role.ID, pageFrom(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}
