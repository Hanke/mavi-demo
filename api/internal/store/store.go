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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

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

type Candidate struct {
	ID         string    `json:"id"`
	FullName   string    `json:"full_name"`
	Email      *string   `json:"email"`
	Phone      *string   `json:"phone"`
	Location   *string   `json:"location"`
	ResumeText string    `json:"resume_text"`
	Source     *string   `json:"source"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

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

func (s *Store) DeleteCandidate(ctx context.Context, id string) error {
	return s.deleteRow(ctx, `DELETE FROM candidates WHERE id = $1`, id)
}

// ---------------------------------------------------------------------------
// Profiles (1:1 with candidates)
// ---------------------------------------------------------------------------

type Profile struct {
	ID              string          `json:"id"`
	CandidateID     string          `json:"candidate_id"`
	Profile         json.RawMessage `json:"profile"`
	Headline        *string         `json:"headline"`
	YearsExperience *int16          `json:"years_experience"`
	Certifications  []string        `json:"certifications"`
	Software        []string        `json:"software"`
	Availability    string          `json:"availability"`
	AvailableFrom   *Date           `json:"available_from"`
	Timezone        *string         `json:"timezone"`
	EmbeddingModel  *string         `json:"embedding_model"`
	EmbeddedAt      *time.Time      `json:"embedded_at"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type ProfileInput struct {
	Profile         json.RawMessage
	Headline        *string
	YearsExperience *int16
	Certifications  []string // canonical taxonomy ids
	Software        []string // canonical taxonomy ids
	Availability    string
	AvailableFrom   *Date
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
	if from != nil {
		d := Date(*from)
		p.AvailableFrom = &d
	}
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
	if in.Profile == nil {
		in.Profile = json.RawMessage(`{}`)
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO candidate_profiles AS cp
			(candidate_id, profile, headline, years_experience, certifications, software, availability, available_from, timezone)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (candidate_id) DO UPDATE SET
			profile = EXCLUDED.profile, headline = EXCLUDED.headline, years_experience = EXCLUDED.years_experience,
			certifications = EXCLUDED.certifications, software = EXCLUDED.software, availability = EXCLUDED.availability,
			available_from = EXCLUDED.available_from, timezone = EXCLUDED.timezone,
			embedding       = CASE WHEN `+profileTextChanged+` THEN NULL ELSE cp.embedding END,
			embedding_model = CASE WHEN `+profileTextChanged+` THEN NULL ELSE cp.embedding_model END,
			embedded_at     = CASE WHEN `+profileTextChanged+` THEN NULL ELSE cp.embedded_at END
		RETURNING `+profileCols+`, (xmax = 0) AS inserted`,
		candidateID, in.Profile, in.Headline, in.YearsExperience, nonNil(in.Certifications), nonNil(in.Software),
		in.Availability, in.AvailableFrom.timePtr(), in.Timezone)
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
// Roles
// ---------------------------------------------------------------------------

type Role struct {
	ID                     string          `json:"id"`
	Title                  string          `json:"title"`
	Company                *string         `json:"company"`
	Description            string          `json:"description"`
	Requirements           json.RawMessage `json:"requirements"`
	MustHaves              []string        `json:"must_haves"`
	NiceToHaves            []string        `json:"nice_to_haves"`
	RequiredCertifications []string        `json:"required_certifications"`
	RequiredSoftware       []string        `json:"required_software"`
	Timezone               *string         `json:"timezone"`
	StartsOn               *Date           `json:"starts_on"`
	Status                 string          `json:"status"`
	EmbeddingModel         *string         `json:"embedding_model"`
	EmbeddedAt             *time.Time      `json:"embedded_at"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
}

type RoleInput struct {
	Title                  string
	Company                *string
	Description            string
	Requirements           json.RawMessage
	MustHaves              []string
	NiceToHaves            []string
	RequiredCertifications []string // canonical taxonomy ids
	RequiredSoftware       []string // canonical taxonomy ids
	Timezone               *string
	StartsOn               *Date
	Status                 string
}

const roleCols = `id::text, title, company, description, requirements, must_haves, nice_to_haves,
	required_certifications, required_software, timezone, starts_on, status, embedding_model, embedded_at, created_at, updated_at`

func scanRole(row pgx.Row) (Role, error) {
	var r Role
	var starts *time.Time
	var must, nice []byte
	err := row.Scan(&r.ID, &r.Title, &r.Company, &r.Description, &r.Requirements, &must, &nice,
		&r.RequiredCertifications, &r.RequiredSoftware, &r.Timezone, &starts, &r.Status, &r.EmbeddingModel, &r.EmbeddedAt,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return r, mapErr(err)
	}
	if err := json.Unmarshal(must, &r.MustHaves); err != nil {
		return r, fmt.Errorf("must_haves: %w", err)
	}
	if err := json.Unmarshal(nice, &r.NiceToHaves); err != nil {
		return r, fmt.Errorf("nice_to_haves: %w", err)
	}
	if starts != nil {
		d := Date(*starts)
		r.StartsOn = &d
	}
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
		nonNil(in.RequiredCertifications), nonNil(in.RequiredSoftware), in.Timezone, in.StartsOn.timePtr(), in.Status}, nil
}

func (s *Store) CreateRole(ctx context.Context, in RoleInput) (Role, error) {
	args, err := roleArgs(in)
	if err != nil {
		return Role{}, err
	}
	return scanRole(s.pool.QueryRow(ctx, `
		INSERT INTO roles (title, company, description, requirements, must_haves, nice_to_haves,
			required_certifications, required_software, timezone, starts_on, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
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
	// Clear the embedding when the text it was computed from changes.
	return scanRole(s.pool.QueryRow(ctx, `
		UPDATE roles SET
			title = $2, company = $3, description = $4, requirements = $5, must_haves = $6, nice_to_haves = $7,
			required_certifications = $8, required_software = $9, timezone = $10, starts_on = $11, status = $12,
			embedding = CASE WHEN description IS DISTINCT FROM $4 THEN NULL ELSE embedding END,
			embedding_model = CASE WHEN description IS DISTINCT FROM $4 THEN NULL ELSE embedding_model END,
			embedded_at = CASE WHEN description IS DISTINCT FROM $4 THEN NULL ELSE embedded_at END
		WHERE id = $1
		RETURNING `+roleCols, append([]any{id}, args...)...))
}

func (s *Store) DeleteRole(ctx context.Context, id string) error {
	return s.deleteRow(ctx, `DELETE FROM roles WHERE id = $1`, id)
}

// ---------------------------------------------------------------------------
// Matches
// ---------------------------------------------------------------------------

type Match struct {
	ID          string          `json:"id"`
	RoleID      string          `json:"role_id"`
	CandidateID string          `json:"candidate_id"`
	Score       float64         `json:"score"`
	Explanation string          `json:"explanation"`
	Breakdown   json.RawMessage `json:"breakdown"`
	Status      string          `json:"status"`
	ReleasedAt  *time.Time      `json:"released_at"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`

	// Denormalised for list views so a shortlist needs one request.
	RoleTitle     string `json:"role_title"`
	CandidateName string `json:"candidate_name"`
}

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

// Date is a calendar day serialised as YYYY-MM-DD.
type Date time.Time

func (d Date) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(d).Format("2006-01-02"))
}

func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return fmt.Errorf("date %q: want YYYY-MM-DD", s)
	}
	*d = Date(t)
	return nil
}

func (d *Date) timePtr() *time.Time {
	if d == nil {
		return nil
	}
	t := time.Time(*d)
	return &t
}

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
