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
	if in.Score < 0 || in.Score > 1 {
		v.add("score", "must be between 0 and 1")
	}
	// A match starts undecided. A decision is a review action (review.go),
	// which is what records it.
	if in.Status != string(contract.MatchStatusProposed) && in.Status != string(contract.MatchStatusPendingReview) {
		v.add("status", "must be proposed or pending_review: "+statusIsReviewed)
	}
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

// statusIsReviewed is why POST and PUT /matches do not take a decision as a
// status.
const statusIsReviewed = "a decision is made with POST /matches/{id}/approve, /reject or /swap, which record who made it"

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
	// MatchUpdate has no role_id / candidate_id / status. They are accepted
	// here only so the answer can say why instead of a generic unknown-field
	// 400: the pair is immutable, and the status changes by review.
	var b struct {
		contract.MatchUpdate
		RoleID      *string `json:"role_id"`
		CandidateID *string `json:"candidate_id"`
		Status      *string `json:"status"`
	}
	sent, ok := decodeBody(w, r, &b, false)
	if !ok {
		return
	}
	if sent.has("role_id") || sent.has("candidate_id") {
		fail(w, &validationError{Fields: map[string]string{"role_id": "cannot change the pair; delete and recreate the match"}})
		return
	}
	if sent.has("status") {
		fail(w, &validationError{Fields: map[string]string{"status": "cannot be edited: " + statusIsReviewed}})
		return
	}
	// Only what was sent is written (store.UpdateMatch), so an edit of one
	// field does not put back the others as they were read a moment ago,
	// over what a matching run has written since.
	v := &validationError{}
	in := store.MatchUpdate{Score: b.Score}
	if b.Score != nil && (*b.Score < 0 || *b.Score > 1) {
		v.add("score", "must be between 0 and 1")
	}
	if sent.has("explanation") {
		explanation := strOr(b.Explanation, "")
		in.Explanation = &explanation
	}
	if sent.has("breakdown") {
		in.Breakdown = jsonObject(v, "breakdown", b.Breakdown)
		if in.Breakdown == nil {
			in.Breakdown = json.RawMessage(`{}`)
		}
	}
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

func (s *Server) deleteMatch(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteMatch(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
