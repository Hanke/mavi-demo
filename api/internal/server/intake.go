package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/tasks"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
)

// maxJDChars is the AI service's own limit on a document's text
// (MAX_TEXT_CHARS in ai/app/main.py). It is applied here as well so the
// employer is told which field is too long rather than shown the service's
// validation error.
const maxJDChars = 60_000

// intakeEmbedTimeout bounds the embedding done inside the intake request: the
// client's embed deadline twice over (a structured document the service will
// not take is embedded again as text), and a little for the queries around it.
const intakeEmbedTimeout = 2*aiclient.DefaultEmbedTimeout + 5*time.Second

// intakeRole is employer intake: the request body carries a pasted job
// description. It is parsed in the request, because the answer is what the
// parser understood, for the employer to check; the role is stored with the
// raw text, the whole extraction and the promoted must-haves; it is embedded;
// and a match_role job (the matching run) is queued for it. A parse that fails stores nothing.
// An embedding that fails does not undo the role: an embed_role job takes
// over, as it would after POST /roles.
func (s *Server) intakeRole(w http.ResponseWriter, r *http.Request) {
	var in contract.RoleIntakeInput
	if _, ok := decodeBody(w, r, &in, false); !ok {
		return
	}
	v := &validationError{}
	switch {
	case strings.TrimSpace(in.Description) == "":
		v.add("description", "required: paste the job description")
	case utf8.RuneCountInString(in.Description) > maxJDChars:
		v.add("description", fmt.Sprintf("longer than %d characters", maxJDChars))
	}
	if err := v.err(); err != nil {
		fail(w, err)
		return
	}
	parsed, err := s.ai.ParseJD(r.Context(), in.Description)
	if err != nil {
		failParseJD(w, err)
		return
	}
	input, err := s.roleFromJD(in, parsed)
	if err != nil {
		fail(w, err)
		return
	}

	// The parse is paid for; from here a client that disconnects must not
	// leave the role half made (stored but not embedded, or with no matching
	// job).
	detached := context.WithoutCancel(r.Context())
	ctx, cancel := context.WithTimeout(detached, 5*time.Second)
	defer cancel()
	role, err := s.store.CreateRole(ctx, input)
	if err != nil {
		fail(w, err)
		return
	}
	if s.embedRoleNow != nil {
		ectx, cancel := context.WithTimeout(detached, intakeEmbedTimeout)
		if err := s.embedRoleNow(ectx, role.ID); err != nil {
			logf("role intake: embed role %s: %v; leaving it to %s", role.ID, err, tasks.KindEmbedRole)
		} else if embedded, err := s.store.GetRole(ectx, role.ID); err == nil {
			role = embedded
		}
		cancel()
	}
	s.embedRole(r, role) // queues embed_role only if the role is still not embedded

	qctx, cancel := context.WithTimeout(detached, 5*time.Second)
	defer cancel()
	job, _, err := s.jobs.Enqueue(qctx, jobs.EnqueueInput{Kind: tasks.KindMatchRole, Payload: roleJobPayload(role.ID)})
	if err != nil {
		// A role nobody will be matched to is not what was asked for, and a
		// retry would add a second one, so this one is taken back.
		if derr := s.store.DeleteRole(qctx, role.ID); derr != nil {
			logf("role intake: role %s has no %s job and could not be removed: %v", role.ID, tasks.KindMatchRole, derr)
		}
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, contract.RoleIntake{Role: role, MatchingJob: job})
}

// roleFromJD maps the parser's output onto the role row: the JD itself as the
// description, the whole extraction as the JSONB document, and the must-haves
// as columns. The request's own title and company win over the parser's. The
// certification and software ids are checked against the API's own taxonomy:
// one it does not know would sit in a column no profile can ever satisfy, so
// it is an error (the two services are reading different versions of
// infra/taxonomy.json), not something to drop. The other hard-filter fields
// come from a model reading somebody else's document, so a value the column
// cannot hold is left out instead, and the employer sets it with PUT.
func (s *Server) roleFromJD(in contract.RoleIntakeInput, parsed aiclient.ParsedJD) (store.RoleInput, error) {
	req := parsed.Requirements
	out := store.RoleInput{
		Description:  in.Description,
		Requirements: parsed.RequirementsJSON,
		MustHaves:    trimAll(req.MustHaves),
		NiceToHaves:  trimAll(req.NiceToHaves),
		Status:       string(contract.RoleStatusOpen),
	}
	if title := strPtr(in.Title); title != nil {
		out.Title = *title
	} else if title := strPtr(req.Title); title != nil {
		out.Title = *title
	} else {
		v := &validationError{}
		v.add("title", "the job description names no title; send one")
		return store.RoleInput{}, v
	}
	if out.Company = strPtr(in.Company); out.Company == nil {
		out.Company = strPtr(parsed.Company)
	}

	var unknownCerts, unknownSoftware []string
	out.RequiredCertifications, unknownCerts = s.tax.ResolveAll(taxonomy.Certifications, req.RequiredCertifications)
	out.RequiredSoftware, unknownSoftware = s.tax.ResolveAll(taxonomy.Software, req.RequiredSoftware)
	if len(unknownCerts)+len(unknownSoftware) > 0 {
		return store.RoleInput{}, fmt.Errorf("role intake: the parser returned ids that are not in the taxonomy (certifications: %v; software: %v)",
			unknownCerts, unknownSoftware)
	}

	within := func(p *int, lo, hi int) *int {
		if p == nil || *p < lo || *p > hi {
			return nil
		}
		return p
	}
	out.MinYearsExperience = within(req.MinYearsExperience, 1, 70)
	out.HoursPerWeek = within(req.HoursPerWeek, 1, maxHoursPerWeek)
	if tz := strPtr(req.Timezone); tz != nil && *tz != "Local" {
		if _, err := time.LoadLocation(*tz); err == nil {
			out.Timezone = tz
			// An overlap is with the role's working day, which needs a zone to be in.
			out.MinOverlapHours = within(req.MinOverlapHours, 1, maxOverlapHours)
		}
	}
	if req.StartsOn != nil {
		starts := contract.Date(req.StartsOn.Time)
		out.StartsOn = &starts
	}
	return out, nil
}

func roleJobPayload(roleID string) json.RawMessage {
	payload, _ := json.Marshal(map[string]string{"role_id": roleID})
	return payload
}

// failParseJD answers for a failed parse. Text the AI service refused is the
// caller's to fix, so its reason is passed on as a 422; anything else (the
// service is down, the model's output did not validate) is a 503: nothing was
// stored and the same request can be sent again.
func failParseJD(w http.ResponseWriter, err error) {
	var ae *aiclient.Error
	if errors.As(err, &ae) && errors.Is(err, aiclient.ErrBadRequest) {
		writeError(w, http.StatusUnprocessableEntity, "the job description could not be parsed: "+ae.Detail)
		return
	}
	if errors.Is(err, context.Canceled) {
		return // client went away
	}
	logf("parse job description: %v", err)
	writeError(w, http.StatusServiceUnavailable, "the job description could not be parsed right now; try again shortly")
}
