package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/colehanke/mavi-demo/api/internal/auth"
	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/store"
)

func (s *Server) createMatch(w http.ResponseWriter, r *http.Request) {
	var b contract.MatchCreate
	sent, ok := decodeBody(w, r, &b, false)
	if !ok {
		return
	}
	v := &validationError{}
	in := store.MatchInput{
		RoleID:      strings.TrimSpace(b.RoleID),
		CandidateID: strings.TrimSpace(b.CandidateID),
		Score:       b.Score,
		Explanation: strOr(b.Explanation, ""),
		Status:      enumOr(b.Status, string(contract.MatchStatusProposed)),
	}
	if !isUUID(in.RoleID) {
		v.add("role_id", "must be a UUID")
	}
	if !isUUID(in.CandidateID) {
		v.add("candidate_id", "must be a UUID")
	}
	if !sent.set("score") {
		v.add("score", "required")
	}
	in.Breakdown = jsonObject(v, "breakdown", b.Breakdown)
	validateMatchFields(v, in.Score, in.Status)
	if err := v.err(); err != nil {
		fail(w, err)
		return
	}
	// A candidate who has not said when and where they can work is excluded
	// from matching, whoever proposes the match. Whether the answers fit the
	// role is ops' call here: GET /roles/{id}/availability shows it.
	if _, err := s.store.GetAvailability(r.Context(), in.CandidateID); errors.Is(err, store.ErrNotFound) {
		v.add("candidate_id", "has not given their time zone, working hours, hours per week and start date, so cannot be matched")
		fail(w, v.err())
		return
	} else if err != nil {
		fail(w, err)
		return
	}
	m, err := s.store.CreateMatch(r.Context(), in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func validateMatchFields(v *validationError, score float64, status string) {
	if score < 0 || score > 1 {
		v.add("score", "must be between 0 and 1")
	}
	validEnum[contract.MatchStatus](v, "status", status)
}

// scopeMatches applies the role's visibility rule to a filter. Employers
// see released matches only. Talent sees released matches for their own
// candidate only. Ops sees everything. It writes the response and returns
// false when the caller cannot see anything.
func scopeMatches(w http.ResponseWriter, r *http.Request, f store.MatchFilter) (store.MatchFilter, bool) {
	id := identity(r)
	switch id.Role {
	case auth.Ops:
		return f, true
	case auth.Employer:
		f.ReleasedOnly = true
		return f, true
	case auth.Talent:
		if !talentActor(w, id) {
			return f, false
		}
		if f.CandidateID != "" && !strings.EqualFold(f.CandidateID, id.Actor) {
			writeJSON(w, http.StatusOK, []store.Match{})
			return f, false
		}
		f.CandidateID = id.Actor
		f.ReleasedOnly = true
		return f, true
	}
	writeError(w, http.StatusForbidden, "role "+string(id.Role)+" may not access matches")
	return f, false
}

func (s *Server) listMatches(w http.ResponseWriter, r *http.Request) {
	v := &validationError{}
	f := store.MatchFilter{RoleID: uuidParam(v, r, "role_id"), CandidateID: uuidParam(v, r, "candidate_id"), Status: r.URL.Query().Get("status")}
	if f.Status != "" {
		validEnum[contract.MatchStatus](v, "status", f.Status)
	}
	if err := v.err(); err != nil {
		fail(w, err)
		return
	}
	f, ok := scopeMatches(w, r, f)
	if !ok {
		return
	}
	out, err := s.store.ListMatches(r.Context(), f, pageFrom(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getMatch(w http.ResponseWriter, r *http.Request) {
	f, ok := scopeMatches(w, r, store.MatchFilter{})
	if !ok {
		return
	}
	m, err := s.store.GetMatch(r.Context(), r.PathValue("id"), f.ReleasedOnly)
	if err != nil {
		fail(w, err)
		return
	}
	if f.CandidateID != "" && !strings.EqualFold(m.CandidateID, f.CandidateID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) updateMatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// MatchUpdate has no role_id / candidate_id. They are accepted here only
	// so the answer can say the pair is immutable instead of a generic
	// unknown-field 400.
	var b struct {
		contract.MatchUpdate
		RoleID      *string `json:"role_id"`
		CandidateID *string `json:"candidate_id"`
	}
	sent, ok := decodeBody(w, r, &b, false)
	if !ok {
		return
	}
	if sent.has("role_id") || sent.has("candidate_id") {
		fail(w, &validationError{Fields: map[string]string{"role_id": "cannot change the pair; delete and recreate the match"}})
		return
	}
	cur, err := s.store.GetMatch(r.Context(), id, false)
	if err != nil {
		fail(w, err)
		return
	}
	v := &validationError{}
	in := store.MatchUpdate{Score: cur.Score, Explanation: cur.Explanation, Breakdown: cur.Breakdown, Status: string(cur.Status)}
	if b.Score != nil {
		in.Score = *b.Score
	}
	if sent.has("explanation") {
		in.Explanation = strOr(b.Explanation, "")
	}
	if b.Status != nil {
		in.Status = string(*b.Status)
	}
	if sent.has("breakdown") {
		in.Breakdown = jsonObject(v, "breakdown", b.Breakdown)
		if in.Breakdown == nil {
			in.Breakdown = json.RawMessage(`{}`)
		}
	}
	validateMatchFields(v, in.Score, in.Status)
	if err := v.err(); err != nil {
		fail(w, err)
		return
	}
	m, err := s.store.UpdateMatch(r.Context(), id, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// releaseMatch flips released_at and records the ops actor in review_events.
func (s *Server) releaseMatch(release bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b contract.ReleaseInput
		if _, ok := decodeBody(w, r, &b, true); !ok {
			return
		}
		actor := identity(r).Actor
		if actor == "" {
			actor = "ops"
		}
		m, err := s.store.SetReleased(r.Context(), r.PathValue("id"), release, actor, strOr(b.Reason, ""))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, m)
	}
}

func (s *Server) deleteMatch(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteMatch(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
