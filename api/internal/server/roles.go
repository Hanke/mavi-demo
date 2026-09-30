package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/colehanke/mavi-demo/api/internal/auth"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
)

type roleBody struct {
	Title                  *string         `json:"title"`
	Company                *string         `json:"company"`
	Description            *string         `json:"description"`
	Requirements           json.RawMessage `json:"requirements"`
	MustHaves              []string        `json:"must_haves"`
	NiceToHaves            []string        `json:"nice_to_haves"`
	RequiredCertifications []string        `json:"required_certifications"`
	RequiredSoftware       []string        `json:"required_software"`
	Timezone               *string         `json:"timezone"`
	StartsOn               *store.Date     `json:"starts_on"`
	Status                 *string         `json:"status"`
}

// apply overlays the body on base (the current row on PUT, zero on POST).
// Taxonomy lists arrive as free text and are stored as canonical ids.
func (s *Server) applyRole(b roleBody, base store.RoleInput, sent body) (store.RoleInput, error) {
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
	if sent.has("timezone") {
		base.Timezone = strPtr(b.Timezone)
	}
	if sent.has("starts_on") {
		base.StartsOn = b.StartsOn
	}
	if sent.has("status") {
		base.Status = strOr(b.Status, "")
	} else if base.Status == "" {
		base.Status = "open"
	}
	if base.Title == "" {
		v.add("title", "required")
	}
	oneOf(v, "status", base.Status, "open", "filled", "closed")
	validTimezone(v, "timezone", base.Timezone)
	return base, v.err()
}

func (s *Server) createRole(w http.ResponseWriter, r *http.Request) {
	var in roleBody
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
	writeJSON(w, http.StatusCreated, role)
}

// talentRoleFilter restricts talent to open roles; other roles see all.
func talentRoleFilter(r *http.Request, f store.RoleFilter) (store.RoleFilter, bool) {
	if identity(r).Role != auth.Talent {
		return f, true
	}
	if f.Status != "" && f.Status != "open" {
		return f, false
	}
	f.Status = "open"
	return f, true
}

func (s *Server) listRoles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := &validationError{}
	f := store.RoleFilter{Status: q.Get("status"), Company: q.Get("company")}
	if f.Status != "" {
		oneOf(v, "status", f.Status, "open", "filled", "closed")
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
	if identity(r).Role == auth.Talent && role.Status != "open" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, role)
}

func (s *Server) updateRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in roleBody
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
		Timezone: cur.Timezone, StartsOn: cur.StartsOn, Status: cur.Status,
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
	writeJSON(w, http.StatusOK, role)
}

func (s *Server) deleteRole(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteRole(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
