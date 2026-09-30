// Package tasks holds the job handlers the worker runs: the slow work the
// API hands to the queue instead of doing inside a request. Today that is
// embedding roles and candidate profiles through the AI service; profile
// extraction and matching runs will register here too.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job kinds. The API enqueues these; a worker built with Registry runs them.
const (
	// KindEmbedRole embeds a role's description. Payload: {"role_id": uuid}.
	KindEmbedRole = "embed_role"
	// KindEmbedProfile embeds a candidate profile. Payload: {"candidate_id": uuid}.
	KindEmbedProfile = "embed_profile"
)

// EmbeddingDim is the width of the vector columns (see 0002_core_tables).
const EmbeddingDim = 1536

// Embedder is the subset of *aiclient.Client the handlers use.
type Embedder interface {
	Embedding(ctx context.Context, text string) (aiclient.EmbedResponse, error)
}

var _ Embedder = (*aiclient.Client)(nil)

// Registry returns every handler, keyed by kind.
func Registry(pool *pgxpool.Pool, ai Embedder) jobs.Registry {
	e := &embedder{pool: pool, ai: ai}
	return jobs.Registry{
		KindEmbedRole:    e.role,
		KindEmbedProfile: e.profile,
	}
}

type embedder struct {
	pool *pgxpool.Pool
	ai   Embedder
}

// role embeds roles.description. The store clears the embedding whenever the
// description changes, so the text embedded here is exactly what the column
// is derived from.
func (e *embedder) role(ctx context.Context, job jobs.Job) error {
	var p struct {
		RoleID string `json:"role_id"`
	}
	if err := json.Unmarshal(job.Payload, &p); err != nil || strings.TrimSpace(p.RoleID) == "" {
		return jobs.PayloadError(job, `{"role_id": uuid}`)
	}
	var text string
	err := e.pool.QueryRow(ctx, `SELECT description FROM roles WHERE id = $1`, p.RoleID).Scan(&text)
	if done, err := rowGone(job, err); done {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return nil // nothing to embed; the column stays NULL
	}
	vec, model, err := e.embed(ctx, text)
	if err != nil {
		return err
	}
	return e.write(ctx, job, `
		UPDATE roles SET embedding = $2::vector, embedding_model = $3, embedded_at = now()
		WHERE id = $1 AND description = $4`, p.RoleID, vec, model, text)
}

// profile embeds a candidate profile from the same fields whose change makes
// the store clear the embedding: headline, the structured profile, and the
// certification and software lists.
func (e *embedder) profile(ctx context.Context, job jobs.Job) error {
	var p struct {
		CandidateID string `json:"candidate_id"`
	}
	if err := json.Unmarshal(job.Payload, &p); err != nil || strings.TrimSpace(p.CandidateID) == "" {
		return jobs.PayloadError(job, `{"candidate_id": uuid}`)
	}
	var headline *string
	var profile []byte
	var certs, software []string
	err := e.pool.QueryRow(ctx, `
		SELECT headline, profile, certifications, software FROM candidate_profiles WHERE candidate_id = $1`,
		p.CandidateID).Scan(&headline, &profile, &certs, &software)
	if done, err := rowGone(job, err); done {
		return err
	}
	if certs == nil {
		certs = []string{} // a nil slice would be sent as NULL and never compare equal below
	}
	if software == nil {
		software = []string{}
	}
	text := profileText(headline, profile, certs, software)
	if text == "" {
		return nil
	}
	vec, model, err := e.embed(ctx, text)
	if err != nil {
		return err
	}
	return e.write(ctx, job, `
		UPDATE candidate_profiles SET embedding = $2::vector, embedding_model = $3, embedded_at = now()
		WHERE candidate_id = $1 AND headline IS NOT DISTINCT FROM $4 AND profile = $5::jsonb
		  AND certifications = $6 AND software = $7`,
		p.CandidateID, vec, model, headline, profile, certs, software)
}

// profileText is the document embedded for a profile. It is deterministic
// so re-embedding unchanged input gives the same vector.
func profileText(headline *string, profile []byte, certs, software []string) string {
	var parts []string
	if headline != nil && strings.TrimSpace(*headline) != "" {
		parts = append(parts, strings.TrimSpace(*headline))
	}
	if body := strings.TrimSpace(string(profile)); body != "" && body != "{}" {
		parts = append(parts, body)
	}
	if len(certs) > 0 {
		parts = append(parts, "certifications: "+strings.Join(certs, ", "))
	}
	if len(software) > 0 {
		parts = append(parts, "software: "+strings.Join(software, ", "))
	}
	return strings.Join(parts, "\n")
}

// embed calls the AI service and formats the vector for pgvector. A 4xx
// from the service means this input can never be embedded, so it is
// permanent; anything else (unreachable, timeout, 5xx) is worth a retry.
func (e *embedder) embed(ctx context.Context, text string) (string, string, error) {
	resp, err := e.ai.Embedding(ctx, text)
	if err != nil {
		if errors.Is(err, aiclient.ErrBadRequest) {
			return "", "", jobs.Permanent(err)
		}
		return "", "", err
	}
	if len(resp.Embedding) != EmbeddingDim {
		return "", "", jobs.Permanent(fmt.Errorf("ai service returned a %d-dimension embedding; the columns are vector(%d)", len(resp.Embedding), EmbeddingDim))
	}
	return vectorLiteral(resp.Embedding), resp.Provider, nil
}

// write stores the vector, but only if the text it was computed from is
// still what the row holds: the WHERE clause compares exactly the columns
// the store derives the embedding from (the same ones whose change makes it
// clear the embedding), so an unrelated edit during the provider call (a
// status change, say) does not get in the way, while a text edit is not
// overwritten with a stale vector. That case comes back as a retryable
// error and the next attempt embeds the new text.
func (e *embedder) write(ctx context.Context, job jobs.Job, sql string, args ...any) error {
	tag, err := e.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("job %d: text changed or row deleted while embedding; retrying with the current text", job.ID)
	}
	return nil
}

// rowGone interprets the read that starts each handler. A row that no longer
// exists is not an error: the job is done, there is nothing to embed. An id
// that is not a UUID is a permanent failure.
func rowGone(job jobs.Job, err error) (bool, error) {
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, pgx.ErrNoRows):
		return true, nil
	case jobs.IsInvalidText(err):
		return true, jobs.PayloadError(job, "a UUID")
	default:
		return true, err
	}
}

// vectorLiteral renders a vector in pgvector's text form, "[0.1,0.2,...]".
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 10)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
