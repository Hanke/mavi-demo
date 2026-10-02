// Package store is the persistence layer for the API's system of record:
// candidates, their profiles, roles and matches. Every query runs against
// the tables in infra/db/migrations; the API is their only writer.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/availability"
	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The row types are the API's response schemas from api/openapi.yaml,
// generated into internal/contract, so what the store scans is exactly what
// the handlers serialise. The *Input types are internal and hand-written.

// Sentinel errors handlers map to HTTP status codes. The constraint ones
// arrive wrapped in a ConstraintError naming the constraint.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")      // unique violation
	ErrInUse    = errors.New("in use")        // delete blocked by a RESTRICT foreign key
	ErrBadRef   = errors.New("bad reference") // foreign key points at a missing row
)

// ConstraintError is a Postgres constraint violation: Kind is one of
// ErrConflict, ErrInUse or ErrBadRef and Constraint is the constraint name.
type ConstraintError struct {
	Kind       error
	Constraint string
}

func (e *ConstraintError) Error() string        { return e.Kind.Error() + ": " + e.Constraint }
func (e *ConstraintError) Is(target error) bool { return target == e.Kind }

// Store runs queries against a pool.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Page bounds a list query.
type Page struct {
	Limit  int
	Offset int
}

func (p Page) clamp() Page {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Limit > 200 {
		p.Limit = 200
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	return p
}

// ---------------------------------------------------------------------------
// Candidates
// ---------------------------------------------------------------------------

type Candidate = contract.Candidate

// CandidateInput is everything a caller may set on a candidate.
type CandidateInput struct {
	FullName   string
	Email      *string
	Phone      *string
	Location   *string
	ResumeText string
	Source     *string
	Status     string
}

const candidateCols = `id::text, full_name, email, phone, location, resume_text, source, status, created_at, updated_at`

func scanCandidate(row pgx.Row) (Candidate, error) {
	var c Candidate
	err := row.Scan(&c.ID, &c.FullName, &c.Email, &c.Phone, &c.Location, &c.ResumeText, &c.Source, &c.Status, &c.CreatedAt, &c.UpdatedAt)
	return c, mapErr(err)
}

func (s *Store) CreateCandidate(ctx context.Context, in CandidateInput) (Candidate, error) {
	return scanCandidate(s.pool.QueryRow(ctx, `
		INSERT INTO candidates (full_name, email, phone, location, resume_text, source, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+candidateCols,
		in.FullName, in.Email, in.Phone, in.Location, in.ResumeText, in.Source, in.Status))
}

func (s *Store) GetCandidate(ctx context.Context, id string) (Candidate, error) {
	return scanCandidate(s.pool.QueryRow(ctx, `SELECT `+candidateCols+` FROM candidates WHERE id = $1`, id))
}

// CandidateFilter narrows ListCandidates.
type CandidateFilter struct {
	Status string // "" for any
}

func (s *Store) ListCandidates(ctx context.Context, f CandidateFilter, p Page) ([]Candidate, error) {
	p = p.clamp()
	rows, err := s.pool.Query(ctx, `
		SELECT `+candidateCols+` FROM candidates
		WHERE ($1 = '' OR status = $1)
		ORDER BY created_at DESC, id
		LIMIT $2 OFFSET $3`, f.Status, p.Limit, p.Offset)
	return collect(rows, err, scanCandidate)
}

func (s *Store) UpdateCandidate(ctx context.Context, id string, in CandidateInput) (Candidate, error) {
	return scanCandidate(s.pool.QueryRow(ctx, `
		UPDATE candidates
		SET full_name = $2, email = $3, phone = $4, location = $5, resume_text = $6, source = $7, status = $8
		WHERE id = $1
		RETURNING `+candidateCols,
		id, in.FullName, in.Email, in.Phone, in.Location, in.ResumeText, in.Source, in.Status))
}

// SetResumeText replaces a candidate's raw resume text, the input to profile
// extraction, and nothing else on the row.
func (s *Store) SetResumeText(ctx context.Context, id, text string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE candidates SET resume_text = $2 WHERE id = $1`, id, text)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteCandidate(ctx context.Context, id string) error {
	return s.deleteRow(ctx, `DELETE FROM candidates WHERE id = $1`, id)
}

// ---------------------------------------------------------------------------
// Profiles (1:1 with candidates)
// ---------------------------------------------------------------------------

type Profile = contract.Profile

type ProfileInput struct {
	Profile         json.RawMessage
	Headline        *string
	YearsExperience *int
	Certifications  []string // canonical taxonomy ids
	Software        []string // canonical taxonomy ids
	Availability    string
	AvailableFrom   *contract.Date
	Timezone        *string
}

const profileCols = `id::text, candidate_id::text, profile, headline, years_experience, certifications, software,
	availability, available_from, timezone, embedding_model, embedded_at, created_at, updated_at`

// scanProfile reads profileCols; extra receives any trailing columns.
func scanProfile(row pgx.Row, extra ...any) (Profile, error) {
	var p Profile
	var from *time.Time
	dest := append([]any{&p.ID, &p.CandidateID, &p.Profile, &p.Headline, &p.YearsExperience, &p.Certifications, &p.Software,
		&p.Availability, &from, &p.Timezone, &p.EmbeddingModel, &p.EmbeddedAt, &p.CreatedAt, &p.UpdatedAt}, extra...)
	err := row.Scan(dest...)
	p.AvailableFrom = contract.DatePtr(from)
	if p.Certifications == nil {
		p.Certifications = []string{}
	}
	if p.Software == nil {
		p.Software = []string{}
	}
	return p, mapErr(err)
}

func (s *Store) GetProfile(ctx context.Context, candidateID string) (Profile, error) {
	return scanProfile(s.pool.QueryRow(ctx, `SELECT `+profileCols+` FROM candidate_profiles WHERE candidate_id = $1`, candidateID))
}

// UpsertProfile creates or replaces the profile for a candidate. inserted
// reports whether a row was created rather than replaced. The embedding is
// cleared only when the text it was computed from changed, so re-saving an
// unchanged profile does not cost another provider call.
func (s *Store) UpsertProfile(ctx context.Context, candidateID string, in ProfileInput) (p Profile, inserted bool, err error) {
	return s.upsertProfile(ctx, candidateID, in, nil)
}

// UpsertParsedProfile is UpsertProfile for a profile extracted from
// resumeText. It writes only while that is still the candidate's resume
// text and reports ErrNotFound otherwise (the resume was replaced, or the
// candidate deleted, while the parser ran), so a slow parse of an old upload
// cannot overwrite the profile of a newer one.
func (s *Store) UpsertParsedProfile(ctx context.Context, candidateID string, in ProfileInput, resumeText string) (p Profile, inserted bool, err error) {
	return s.upsertProfile(ctx, candidateID, in, &resumeText)
}

func (s *Store) upsertProfile(ctx context.Context, candidateID string, in ProfileInput, resumeText *string) (p Profile, inserted bool, err error) {
	if in.Profile == nil {
		in.Profile = json.RawMessage(`{}`)
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO candidate_profiles AS cp
			(candidate_id, profile, headline, years_experience, certifications, software, availability, available_from, timezone)
		SELECT $1::uuid, $2::jsonb, $3::text, $4::smallint, $5::text[], $6::text[], $7::text, $8::date, $9::text
		WHERE $10::text IS NULL OR EXISTS (SELECT 1 FROM candidates WHERE id = $1::uuid AND resume_text = $10::text)
		ON CONFLICT (candidate_id) DO UPDATE SET
			profile = EXCLUDED.profile, headline = EXCLUDED.headline, years_experience = EXCLUDED.years_experience,
			certifications = EXCLUDED.certifications, software = EXCLUDED.software, availability = EXCLUDED.availability,
			available_from = EXCLUDED.available_from, timezone = EXCLUDED.timezone,
			embedding       = CASE WHEN `+profileTextChanged+` THEN NULL ELSE cp.embedding END,
			embedding_model = CASE WHEN `+profileTextChanged+` THEN NULL ELSE cp.embedding_model END,
			embedded_at     = CASE WHEN `+profileTextChanged+` THEN NULL ELSE cp.embedded_at END
		RETURNING `+profileCols+`, (xmax = 0) AS inserted`,
		candidateID, in.Profile, in.Headline, in.YearsExperience, nonNil(in.Certifications), nonNil(in.Software),
		in.Availability, in.AvailableFrom.TimePtr(), in.Timezone, resumeText)
	p, err = scanProfile(row, &inserted)
	return p, inserted, err
}

// profileTextChanged is true inside the upsert's DO UPDATE when any column
// the embedding is derived from differs from the stored row.
const profileTextChanged = `(cp.profile IS DISTINCT FROM EXCLUDED.profile OR cp.headline IS DISTINCT FROM EXCLUDED.headline
	OR cp.certifications IS DISTINCT FROM EXCLUDED.certifications OR cp.software IS DISTINCT FROM EXCLUDED.software)`

func (s *Store) DeleteProfile(ctx context.Context, candidateID string) error {
	return s.deleteRow(ctx, `DELETE FROM candidate_profiles WHERE candidate_id = $1`, candidateID)
}

// ---------------------------------------------------------------------------
// Availability (what the candidate said; 0 or 1 row per candidate)
// ---------------------------------------------------------------------------

type WorkAvailability = contract.WorkAvailability

// AvailabilityInput is the four answers, already validated.
type AvailabilityInput struct {
	Timezone      string
	WorkStart     string // HH:MM
	WorkEnd       string // HH:MM
	HoursPerWeek  int
	AvailableFrom contract.Date
}

const availabilityCols = `candidate_id::text, timezone, to_char(work_start, 'HH24:MI'), to_char(work_end, 'HH24:MI'),
	hours_per_week, available_from, created_at, updated_at`

func scanAvailability(row pgx.Row, extra ...any) (WorkAvailability, error) {
	var a WorkAvailability
	var from time.Time
	dest := append([]any{&a.CandidateID, &a.Timezone, &a.WorkStart, &a.WorkEnd, &a.HoursPerWeek, &from, &a.CreatedAt, &a.UpdatedAt}, extra...)
	err := row.Scan(dest...)
	a.AvailableFrom = contract.Date(from)
	return a, mapErr(err)
}

// GetAvailability is ErrNotFound for a candidate who has not supplied it.
func (s *Store) GetAvailability(ctx context.Context, candidateID string) (WorkAvailability, error) {
	return scanAvailability(s.pool.QueryRow(ctx, `SELECT `+availabilityCols+` FROM candidate_availability WHERE candidate_id = $1`, candidateID))
}

// UpsertAvailability stores or replaces a candidate's answers. inserted
// reports whether they were stored for the first time. A candidate that does
// not exist is ErrBadRef.
func (s *Store) UpsertAvailability(ctx context.Context, candidateID string, in AvailabilityInput) (a WorkAvailability, inserted bool, err error) {
	from := time.Time(in.AvailableFrom)
	a, err = scanAvailability(s.pool.QueryRow(ctx, `
		INSERT INTO candidate_availability (candidate_id, timezone, work_start, work_end, hours_per_week, available_from)
		VALUES ($1, $2, $3::time, $4::time, $5, $6)
		ON CONFLICT (candidate_id) DO UPDATE SET
			timezone = EXCLUDED.timezone, work_start = EXCLUDED.work_start, work_end = EXCLUDED.work_end,
			hours_per_week = EXCLUDED.hours_per_week, available_from = EXCLUDED.available_from
		RETURNING `+availabilityCols+`, (xmax = 0) AS inserted`,
		candidateID, in.Timezone, in.WorkStart, in.WorkEnd, in.HoursPerWeek, from), &inserted)
	return a, inserted, err
}

// AvailabilityFilter runs the availability hard filter (package availability)
// for a role over one page of the active candidates, by name: one check per
// candidate, passing or not, with the reasons. A candidate with no
// candidate_availability row is in the result as failed, never left out and
// never passed. on is the day the time-zone overlap is worked out for.
//
// It is paged for the ops listing. A matching run should not page through
// this; it applies availability.Check to the rows of its own shortlist query.
func (s *Store) AvailabilityFilter(ctx context.Context, role Role, on time.Time, p Page) ([]contract.AvailabilityCheck, error) {
	p = p.clamp()
	rows, err := s.pool.Query(ctx, `
		SELECT c.id::text, c.full_name, a.timezone,
		       (extract(epoch FROM a.work_start) / 60)::int, (extract(epoch FROM a.work_end) / 60)::int,
		       a.hours_per_week, a.available_from
		FROM candidates c
		LEFT JOIN candidate_availability a ON a.candidate_id = c.id
		WHERE c.status = 'active'
		ORDER BY c.full_name, c.id
		LIMIT $1 OFFSET $2`, p.Limit, p.Offset)
	req := availability.Role{
		Timezone: role.Timezone, MinOverlapHours: role.MinOverlapHours, HoursPerWeek: role.HoursPerWeek,
		StartsOn: role.StartsOn.TimePtr(),
	}
	return collect(rows, err, func(row pgx.Row) (contract.AvailabilityCheck, error) {
		var out contract.AvailabilityCheck
		var tz *string
		var start, end, hours *int
		var from *time.Time
		if err := row.Scan(&out.CandidateID, &out.CandidateName, &tz, &start, &end, &hours, &from); err != nil {
			return out, mapErr(err)
		}
		var cand *availability.Candidate
		if tz != nil {
			cand = &availability.Candidate{Timezone: *tz, WorkStart: *start, WorkEnd: *end, HoursPerWeek: *hours, AvailableFrom: *from}
		}
		v := availability.Check(req, cand, on)
		out.Passed, out.Reasons = v.Passed, nonNil(v.Reasons)
		if v.OverlapMinutes != nil {
			h := float64(*v.OverlapMinutes) / 60
			out.OverlapHours = &h
		}
		return out, nil
	})
}

// ---------------------------------------------------------------------------
// Roles
// ---------------------------------------------------------------------------

type Role = contract.Role

type RoleInput struct {
	Title                  string
	Company                *string
	Description            string
	Requirements           json.RawMessage
	MustHaves              []string
	NiceToHaves            []string
	RequiredCertifications []string // canonical taxonomy ids
	RequiredSoftware       []string // canonical taxonomy ids
	MinYearsExperience     *int
	Timezone               *string
	MinOverlapHours        *int
	HoursPerWeek           *int
	StartsOn               *contract.Date
	Status                 string
}

const roleCols = `id::text, title, company, description, requirements, must_haves, nice_to_haves,
	required_certifications, required_software, min_years_experience, timezone, min_overlap_hours, hours_per_week,
	starts_on, status, embedding_model, embedded_at, created_at, updated_at`

func scanRole(row pgx.Row) (Role, error) {
	var r Role
	var starts *time.Time
	var must, nice []byte
	err := row.Scan(&r.ID, &r.Title, &r.Company, &r.Description, &r.Requirements, &must, &nice,
		&r.RequiredCertifications, &r.RequiredSoftware, &r.MinYearsExperience, &r.Timezone, &r.MinOverlapHours, &r.HoursPerWeek,
		&starts, &r.Status,
		&r.EmbeddingModel, &r.EmbeddedAt, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return r, mapErr(err)
	}
	if err := json.Unmarshal(must, &r.MustHaves); err != nil {
		return r, fmt.Errorf("must_haves: %w", err)
	}
	if err := json.Unmarshal(nice, &r.NiceToHaves); err != nil {
		return r, fmt.Errorf("nice_to_haves: %w", err)
	}
	r.StartsOn = contract.DatePtr(starts)
	r.MustHaves = nonNil(r.MustHaves)
	r.NiceToHaves = nonNil(r.NiceToHaves)
	r.RequiredCertifications = nonNil(r.RequiredCertifications)
	r.RequiredSoftware = nonNil(r.RequiredSoftware)
	return r, nil
}

func roleArgs(in RoleInput) ([]any, error) {
	if in.Requirements == nil {
		in.Requirements = json.RawMessage(`{}`)
	}
	must, err := json.Marshal(nonNil(in.MustHaves))
	if err != nil {
		return nil, err
	}
	nice, err := json.Marshal(nonNil(in.NiceToHaves))
	if err != nil {
		return nil, err
	}
	return []any{in.Title, in.Company, in.Description, in.Requirements, must, nice,
		nonNil(in.RequiredCertifications), nonNil(in.RequiredSoftware), in.MinYearsExperience, in.Timezone,
		in.StartsOn.TimePtr(), in.Status, in.MinOverlapHours, in.HoursPerWeek}, nil
}

func (s *Store) CreateRole(ctx context.Context, in RoleInput) (Role, error) {
	args, err := roleArgs(in)
	if err != nil {
		return Role{}, err
	}
	return scanRole(s.pool.QueryRow(ctx, `
		INSERT INTO roles (title, company, description, requirements, must_haves, nice_to_haves,
			required_certifications, required_software, min_years_experience, timezone, starts_on, status,
			min_overlap_hours, hours_per_week)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING `+roleCols, args...))
}

func (s *Store) GetRole(ctx context.Context, id string) (Role, error) {
	return scanRole(s.pool.QueryRow(ctx, `SELECT `+roleCols+` FROM roles WHERE id = $1`, id))
}

type RoleFilter struct {
	Status  string // "" for any
	Company string // "" for any; exact match
}

func (s *Store) ListRoles(ctx context.Context, f RoleFilter, p Page) ([]Role, error) {
	p = p.clamp()
	rows, err := s.pool.Query(ctx, `
		SELECT `+roleCols+` FROM roles
		WHERE ($1 = '' OR status = $1) AND ($2 = '' OR company = $2)
		ORDER BY created_at DESC, id
		LIMIT $3 OFFSET $4`, f.Status, f.Company, p.Limit, p.Offset)
	return collect(rows, err, scanRole)
}

func (s *Store) UpdateRole(ctx context.Context, id string, in RoleInput) (Role, error) {
	args, err := roleArgs(in)
	if err != nil {
		return Role{}, err
	}
	// Clear the embedding when anything it was computed from changes (roleTextChanged).
	return scanRole(s.pool.QueryRow(ctx, `
		UPDATE roles SET
			title = $2, company = $3, description = $4, requirements = $5, must_haves = $6, nice_to_haves = $7,
			required_certifications = $8, required_software = $9, min_years_experience = $10, timezone = $11,
			starts_on = $12, status = $13, min_overlap_hours = $14, hours_per_week = $15,
			embedding = CASE WHEN `+roleTextChanged+` THEN NULL ELSE embedding END,
			embedding_model = CASE WHEN `+roleTextChanged+` THEN NULL ELSE embedding_model END,
			embedded_at = CASE WHEN `+roleTextChanged+` THEN NULL ELSE embedded_at END
		WHERE id = $1
		RETURNING `+roleCols, append([]any{id}, args...)...))
}

// roleTextChanged is true inside UpdateRole's SET when any column the
// embedding is derived from differs from the stored row: the title and the
// structured requirements the canonical role text is rendered from, and the
// description a role without them is embedded from (tasks.KindEmbedRole).
const roleTextChanged = `(title IS DISTINCT FROM $2 OR description IS DISTINCT FROM $4
	OR requirements IS DISTINCT FROM $5::jsonb OR must_haves IS DISTINCT FROM $6::jsonb
	OR nice_to_haves IS DISTINCT FROM $7::jsonb OR required_certifications IS DISTINCT FROM $8
	OR required_software IS DISTINCT FROM $9)`

func (s *Store) DeleteRole(ctx context.Context, id string) error {
	return s.deleteRow(ctx, `DELETE FROM roles WHERE id = $1`, id)
}

// ---------------------------------------------------------------------------
// Matches
// ---------------------------------------------------------------------------

type Match = contract.Match

type MatchInput struct {
	RoleID      string
	CandidateID string
	Score       float64
	Explanation string
	Breakdown   json.RawMessage
	Status      string
}

// MatchFilter narrows ListMatches. ReleasedOnly is the employer scope: when
// set, the query never returns an unreleased row regardless of the other
// fields.
type MatchFilter struct {
	RoleID       string
	CandidateID  string
	Status       string
	ReleasedOnly bool
}

const matchCols = `m.id::text, m.role_id::text, m.candidate_id::text, m.score, m.explanation, m.breakdown, m.status,
	m.released_at, m.created_at, m.updated_at, r.title, c.full_name`

const matchFrom = ` FROM matches m JOIN roles r ON r.id = m.role_id JOIN candidates c ON c.id = m.candidate_id `

func scanMatch(row pgx.Row) (Match, error) {
	var m Match
	err := row.Scan(&m.ID, &m.RoleID, &m.CandidateID, &m.Score, &m.Explanation, &m.Breakdown, &m.Status,
		&m.ReleasedAt, &m.CreatedAt, &m.UpdatedAt, &m.RoleTitle, &m.CandidateName)
	return m, mapErr(err)
}

func (s *Store) CreateMatch(ctx context.Context, in MatchInput) (Match, error) {
	if in.Breakdown == nil {
		in.Breakdown = json.RawMessage(`{}`)
	}
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO matches (role_id, candidate_id, score, explanation, breakdown, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text`,
		in.RoleID, in.CandidateID, in.Score, in.Explanation, in.Breakdown, in.Status).Scan(&id)
	if err != nil {
		return Match{}, mapErr(err)
	}
	return s.GetMatch(ctx, id, false)
}

// GetMatch returns one match. With releasedOnly, an unreleased match is
// reported as ErrNotFound so an employer cannot tell it exists.
func (s *Store) GetMatch(ctx context.Context, id string, releasedOnly bool) (Match, error) {
	return scanMatch(s.pool.QueryRow(ctx, `SELECT `+matchCols+matchFrom+`
		WHERE m.id = $1 AND (NOT $2 OR m.released_at IS NOT NULL)`, id, releasedOnly))
}

func (s *Store) ListMatches(ctx context.Context, f MatchFilter, p Page) ([]Match, error) {
	p = p.clamp()
	rows, err := s.pool.Query(ctx, `SELECT `+matchCols+matchFrom+`
		WHERE (NULLIF($1, '') IS NULL OR m.role_id = NULLIF($1, '')::uuid)
		  AND (NULLIF($2, '') IS NULL OR m.candidate_id = NULLIF($2, '')::uuid)
		  AND ($3 = '' OR m.status = $3)
		  AND (NOT $4 OR m.released_at IS NOT NULL)
		ORDER BY m.role_id, m.score DESC, m.id
		LIMIT $5 OFFSET $6`, f.RoleID, f.CandidateID, f.Status, f.ReleasedOnly, p.Limit, p.Offset)
	return collect(rows, err, scanMatch)
}

// MatchUpdate is the ops-editable part of a match.
type MatchUpdate struct {
	Score       float64
	Explanation string
	Breakdown   json.RawMessage
	Status      string
}

func (s *Store) UpdateMatch(ctx context.Context, id string, in MatchUpdate) (Match, error) {
	if in.Breakdown == nil {
		in.Breakdown = json.RawMessage(`{}`)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE matches SET score = $2, explanation = $3, breakdown = $4, status = $5 WHERE id = $1`,
		id, in.Score, in.Explanation, in.Breakdown, in.Status)
	if err != nil {
		return Match{}, mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return Match{}, ErrNotFound
	}
	return s.GetMatch(ctx, id, false)
}

// SetReleased releases or un-releases a match and records who did it. Both
// writes commit together so the audit trail cannot drift from the flag.
func (s *Store) SetReleased(ctx context.Context, id string, released bool, actor, reason string) (Match, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Match{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Only rows whose flag actually flips are updated, so a repeated release
	// (a double-click, a retried request) is a no-op with no audit row.
	tag, err := tx.Exec(ctx, `
		UPDATE matches SET released_at = CASE WHEN $2 THEN now() ELSE NULL END
		WHERE id = $1 AND (released_at IS NULL) = $2`, id, released)
	if err != nil {
		return Match{}, mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return s.GetMatch(ctx, id, false) // ErrNotFound if it does not exist; unchanged otherwise
	}
	action := "release"
	if !released {
		action = "unrelease"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO review_events (match_id, action, actor, reason) VALUES ($1, $2, $3, NULLIF($4, ''))`,
		id, action, actor, reason); err != nil {
		return Match{}, mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Match{}, err
	}
	return s.GetMatch(ctx, id, false)
}

func (s *Store) DeleteMatch(ctx context.Context, id string) error {
	return s.deleteRow(ctx, `DELETE FROM matches WHERE id = $1`, id)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Store) deleteRow(ctx context.Context, sql, id string) error {
	tag, err := s.pool.Exec(ctx, sql, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// collect drains rows through scan. The query error is taken as a parameter
// so every list method reports Postgres errors through mapErr the same way
// the single-row methods do.
func collect[T any](rows pgx.Rows, err error, scan func(pgx.Row) (T, error)) ([]T, error) {
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, mapErr(rows.Err())
}

// mapErr turns pgx / Postgres errors into the package sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return &ConstraintError{Kind: ErrConflict, Constraint: pgErr.ConstraintName}
		case "23503": // foreign_key_violation
			// A delete blocked by RESTRICT reports the referencing table; an
			// insert with a dangling reference reports the referenced one.
			if strings.HasPrefix(pgErr.Message, "update or delete") {
				return &ConstraintError{Kind: ErrInUse, Constraint: pgErr.ConstraintName}
			}
			return &ConstraintError{Kind: ErrBadRef, Constraint: pgErr.ConstraintName}
		case "22P02": // invalid_text_representation, e.g. a non-UUID id
			return ErrNotFound
		}
	}
	return err
}
