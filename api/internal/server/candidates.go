package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/colehanke/mavi-demo/api/internal/auth"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
)

// candidateBody is the JSON a client sends. Pointer fields distinguish
// "omitted" (keep the current value on PUT) from an explicit null (clear).
type candidateBody struct {
	FullName   *string `json:"full_name"`
	Email      *string `json:"email"`
	Phone      *string `json:"phone"`
	Location   *string `json:"location"`
	ResumeText *string `json:"resume_text"`
	Source     *string `json:"source"`
	Status     *string `json:"status"`
}

// apply overlays the body on base and validates the result.
func (b candidateBody) apply(base store.CandidateInput, sent body) (store.CandidateInput, error) {
	v := &validationError{}
	// Nullable fields: an explicit null clears them.
	if sent.has("email") {
		base.Email = strPtr(b.Email)
	}
	if sent.has("phone") {
		base.Phone = strPtr(b.Phone)
	}
	if sent.has("location") {
		base.Location = strPtr(b.Location)
	}
	if sent.has("source") {
		base.Source = strPtr(b.Source)
	}
	if sent.has("resume_text") {
		base.ResumeText = strOr(b.ResumeText, "")
	}
	// Required fields: an explicit null is a validation error, not a no-op.
	if sent.has("full_name") {
		base.FullName = strings.TrimSpace(strOr(b.FullName, ""))
	}
	if sent.has("status") {
		base.Status = strOr(b.Status, "")
	} else if base.Status == "" {
		base.Status = "active"
	}
	if base.FullName == "" {
		v.add("full_name", "required")
	}
	if base.Email != nil && !strings.Contains(*base.Email, "@") {
		v.add("email", "must contain @")
	}
	oneOf(v, "status", base.Status, "active", "archived")
	return base, v.err()
}

func (s *Server) createCandidate(w http.ResponseWriter, r *http.Request) {
	var in candidateBody
	sent, ok := decodeBody(w, r, &in, false)
	if !ok {
		return
	}
	input, err := in.apply(store.CandidateInput{}, sent)
	if err != nil {
		fail(w, err)
		return
	}
	if id := identity(r); id.Role == auth.Talent && input.Source == nil {
		src := "self"
		input.Source = &src
	}
	c, err := s.store.CreateCandidate(r.Context(), input)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func strOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

func (s *Server) listCandidates(w http.ResponseWriter, r *http.Request) {
	v := &validationError{}
	status := r.URL.Query().Get("status")
	if status != "" {
		oneOf(v, "status", status, "active", "archived")
	}
	if err := v.err(); err != nil {
		fail(w, err)
		return
	}
	out, err := s.store.ListCandidates(r.Context(), store.CandidateFilter{Status: status}, pageFrom(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getCandidate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ownsCandidate(w, r, id) {
		return
	}
	c, err := s.store.GetCandidate(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) updateCandidate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ownsCandidate(w, r, id) {
		return
	}
	var in candidateBody
	sent, ok := decodeBody(w, r, &in, false)
	if !ok {
		return
	}
	cur, err := s.store.GetCandidate(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	input, err := in.apply(store.CandidateInput{
		FullName: cur.FullName, Email: cur.Email, Phone: cur.Phone, Location: cur.Location,
		ResumeText: cur.ResumeText, Source: cur.Source, Status: cur.Status,
	}, sent)
	if err != nil {
		fail(w, err)
		return
	}
	c, err := s.store.UpdateCandidate(r.Context(), id, input)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteCandidate(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteCandidate(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Profiles
// ---------------------------------------------------------------------------

// profileBody accepts taxonomy values as free text ("QuickBooks Online") and
// stores the canonical ids; anything the taxonomy does not know is a 422 so
// it can never silently miss a hard filter.
type profileBody struct {
	Profile         json.RawMessage `json:"profile"`
	Headline        *string         `json:"headline"`
	YearsExperience *int16          `json:"years_experience"`
	Certifications  []string        `json:"certifications"`
	Software        []string        `json:"software"`
	Availability    string          `json:"availability"`
	AvailableFrom   *store.Date     `json:"available_from"`
	Timezone        *string         `json:"timezone"`
}

func (s *Server) profileInput(b profileBody) (store.ProfileInput, error) {
	v := &validationError{}
	in := store.ProfileInput{
		Profile:         jsonObject(v, "profile", b.Profile),
		Headline:        strPtr(b.Headline),
		YearsExperience: b.YearsExperience,
		Certifications:  s.resolveTerms(v, "certifications", taxonomy.Certifications, trimAll(b.Certifications)),
		Software:        s.resolveTerms(v, "software", taxonomy.Software, trimAll(b.Software)),
		Availability:    b.Availability,
		AvailableFrom:   b.AvailableFrom,
		Timezone:        strPtr(b.Timezone),
	}
	if in.Availability == "" {
		in.Availability = "unknown"
	}
	oneOf(v, "availability", in.Availability, "immediate", "two_weeks", "one_month", "unavailable", "unknown")
	if in.YearsExperience != nil && (*in.YearsExperience < 0 || *in.YearsExperience > 70) {
		v.add("years_experience", "must be between 0 and 70")
	}
	validTimezone(v, "timezone", in.Timezone)
	return in, v.err()
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ownsCandidate(w, r, id) {
		return
	}
	p, err := s.store.GetProfile(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) putProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ownsCandidate(w, r, id) {
		return
	}
	var in profileBody
	if _, ok := decodeBody(w, r, &in, false); !ok {
		return
	}
	input, err := s.profileInput(in)
	if err != nil {
		fail(w, err)
		return
	}
	p, inserted, err := s.store.UpsertProfile(r.Context(), id, input)
	if errors.Is(err, store.ErrBadRef) {
		// The candidate is the path resource, so a dangling reference is a 404.
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	code := http.StatusOK
	if inserted {
		code = http.StatusCreated
	}
	writeJSON(w, code, p)
}

func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteProfile(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
