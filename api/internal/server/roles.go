package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/colehanke/mavi-demo/api/internal/auth"
	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/tasks"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
)

// applyRole overlays a decoded RoleInput on base (the current row on PUT,
// zero on POST). Taxonomy lists arrive as free text and are stored as
// canonical ids.
func (s *Server) applyRole(b contract.RoleInput, base store.RoleInput, sent body) (store.RoleInput, error) {
	v := &validationError{}
	if sent.has("title") {
		base.Title = strings.TrimSpace(strOr(b.Title, ""))
	}
	if sent.has("company") {
		base.Company = strPtr(b.Company)
	}
	if sent.has("description") {
		base.Description = strOr(b.Description, "")
	}
	if sent.has("requirements") {
		base.Requirements = jsonObject(v, "requirements", b.Requirements)
	}
	if sent.has("must_haves") {
		base.MustHaves = trimAll(b.MustHaves)
	}
	if sent.has("nice_to_haves") {
		base.NiceToHaves = trimAll(b.NiceToHaves)
	}
	if sent.has("required_certifications") {
		base.RequiredCertifications = s.resolveTerms(v, "required_certifications", taxonomy.Certifications, trimAll(b.RequiredCertifications))
	}
	if sent.has("required_software") {
		base.RequiredSoftware = s.resolveTerms(v, "required_software", taxonomy.Software, trimAll(b.RequiredSoftware))
	}
	if sent.has("min_years_experience") {
		base.MinYearsExperience = b.MinYearsExperience
		// No minimum is NULL: a stored 0 would still fail every profile with no years in the shortlist query.
		if base.MinYearsExperience != nil && *base.MinYearsExperience == 0 {
			base.MinYearsExperience = nil
		}
	}
	if sent.has("timezone") {
		base.Timezone = strPtr(b.Timezone)
	}
	// As with the minimum years, 0 is "does not ask" and is stored as NULL.
	if sent.has("min_overlap_hours") {
		base.MinOverlapHours = positive(b.MinOverlapHours)
	}
	if sent.has("hours_per_week") {
		base.HoursPerWeek = positive(b.HoursPerWeek)
	}
	if sent.has("starts_on") {
		base.StartsOn = b.StartsOn
	}
	if sent.has("status") {
		base.Status = enumOr(b.Status, "")
	} else if base.Status == "" {
		base.Status = string(contract.RoleStatusOpen)
	}
	if base.Title == "" {
		v.add("title", "required")
	}
	validEnum[contract.RoleStatus](v, "status", base.Status)
	if base.MinYearsExperience != nil && (*base.MinYearsExperience < 0 || *base.MinYearsExperience > 70) {
		v.add("min_years_experience", "must be between 0 and 70")
	}
	validTimezone(v, "timezone", base.Timezone)
	if h := base.MinOverlapHours; h != nil && (*h < 0 || *h > maxOverlapHours) {
		v.add("min_overlap_hours", fmt.Sprintf("must be between 0 and %d, the length of the role's working day", maxOverlapHours))
	} else if h != nil && base.Timezone == nil {
		// An overlap is with the role's working day, which needs a zone to be in.
		v.add("timezone", "required when min_overlap_hours is set")
	}
	if h := base.HoursPerWeek; h != nil && (*h < 0 || *h > maxHoursPerWeek) {
		v.add("hours_per_week", fmt.Sprintf("must be between 0 and %d", maxHoursPerWeek))
	}
	return base, v.err()
}

// positive is nil for a missing or zero value.
func positive(p *int) *int {
	if p == nil || *p == 0 {
		return nil
	}
	return p
}

func (s *Server) createRole(w http.ResponseWriter, r *http.Request) {
	var in contract.RoleInput
	sent, ok := decodeBody(w, r, &in, false)
	if !ok {
		return
	}
	input, err := s.applyRole(in, store.RoleInput{}, sent)
	if err != nil {
		fail(w, err)
		return
	}
	role, err := s.store.CreateRole(r.Context(), input)
	if err != nil {
		fail(w, err)
		return
	}
	s.embedRole(r, role)
	writeJSON(w, http.StatusCreated, role)
}

// talentRoleFilter restricts talent to open roles; other roles see all.
func talentRoleFilter(r *http.Request, f store.RoleFilter) (store.RoleFilter, bool) {
	if identity(r).Role != auth.Talent {
		return f, true
	}
	open := string(contract.RoleStatusOpen)
	if f.Status != "" && f.Status != open {
		return f, false
	}
	f.Status = open
	return f, true
}

func (s *Server) listRoles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := &validationError{}
	f := store.RoleFilter{Status: q.Get("status"), Company: q.Get("company")}
	if f.Status != "" {
		validEnum[contract.RoleStatus](v, "status", f.Status)
	}
	if err := v.err(); err != nil {
		fail(w, err)
		return
	}
	f, ok := talentRoleFilter(r, f)
	if !ok {
		writeJSON(w, http.StatusOK, []store.Role{})
		return
	}
	out, err := s.store.ListRoles(r.Context(), f, pageFrom(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getRole(w http.ResponseWriter, r *http.Request) {
	role, err := s.store.GetRole(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if identity(r).Role == auth.Talent && role.Status != contract.RoleStatusOpen {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, role)
}

func (s *Server) updateRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in contract.RoleInput
	sent, ok := decodeBody(w, r, &in, false)
	if !ok {
		return
	}
	cur, err := s.store.GetRole(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	input, err := s.applyRole(in, store.RoleInput{
		Title: cur.Title, Company: cur.Company, Description: cur.Description, Requirements: cur.Requirements,
		MustHaves: cur.MustHaves, NiceToHaves: cur.NiceToHaves,
		RequiredCertifications: cur.RequiredCertifications, RequiredSoftware: cur.RequiredSoftware,
		MinYearsExperience: cur.MinYearsExperience,
		Timezone:           cur.Timezone, MinOverlapHours: cur.MinOverlapHours, HoursPerWeek: cur.HoursPerWeek,
		StartsOn: cur.StartsOn, Status: string(cur.Status),
	}, sent)
	if err != nil {
		fail(w, err)
		return
	}
	role, err := s.store.UpdateRole(r.Context(), id, input)
	if err != nil {
		fail(w, err)
		return
	}
	s.embedRole(r, role)
	writeJSON(w, http.StatusOK, role)
}

func (s *Server) deleteRole(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteRole(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// embedRole queues an embedding job when a write left the role without an
// embedding and there is something to embed: structured requirements, or
// failing those a description. The store clears the embedding only when one
// of those (or the title) changes, so an edit that leaves them in place
// queues nothing.
func (s *Server) embedRole(r *http.Request, role store.Role) {
	structured := tasks.RoleStructured(role.Requirements, role.MustHaves, role.NiceToHaves, role.RequiredCertifications, role.RequiredSoftware)
	if role.EmbeddedAt == nil && (structured || strings.TrimSpace(role.Description) != "") {
		s.enqueueEmbedding(r, tasks.KindEmbedRole, "role_id", role.ID)
	}
}
