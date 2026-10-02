// Package tasks holds the job handlers the worker runs: the slow work the
// API hands to the queue instead of doing inside a request. Today that is
// embedding roles and candidate profiles through the AI service (this file)
// and extracting a profile from an uploaded resume (resume.go). Matching runs
// will register here too; their kind is already named (KindMatchRole) because
// role intake queues one.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job kinds. The API enqueues these; a worker built with Registry runs them.
const (
	// KindEmbedRole embeds a role's description. Payload: {"role_id": uuid}.
	KindEmbedRole = "embed_role"
	// KindEmbedProfile embeds a candidate profile. Payload: {"candidate_id": uuid}.
	KindEmbedProfile = "embed_profile"
	// KindParseResume extracts a candidate's profile from their resume text,
	// stores it and embeds it. Payload: {"candidate_id": uuid}.
	KindParseResume = "parse_resume"
	// KindMatchRole is a matching run for a role: shortlist, rerank, write the
	// matches. Payload: {"role_id": uuid}. Role intake (POST /roles/intake)
	// queues it, but it has no handler in Registry yet, so no worker claims
	// it and the job waits until the handler lands. Its first two stages
	// exist (HardFilter: the hard filters and the retrieval by embedding);
	// the rest takes its candidates from that run's Retrieved and from
	// nowhere else. That handler must treat a role whose embedding is still
	// NULL (the run's RoleEmbedded is false) as not ready and retry:
	// intake embeds before it queues the run, but a failed embedding is
	// handed to an embed_role job that may finish later.
	KindMatchRole = "match_role"
)

// EmbeddingDim is the width of the vector columns (see 0002_core_tables).
const EmbeddingDim = 1536

// AI is the subset of *aiclient.Client the handlers use.
type AI interface {
	Embedder
	ParseResume(ctx context.Context, text string) (aiclient.ParsedResume, error)
}

var _ AI = (*aiclient.Client)(nil)

// Registry returns every handler, keyed by kind. tax is what parse_resume
// checks the parser's certification and software ids against; the embedding
// handlers do not use it.
func Registry(pool *pgxpool.Pool, ai AI, tax *taxonomy.Taxonomy) jobs.Registry {
	e := &embedder{pool: pool, ai: ai}
	r := &resumeParser{pool: pool, ai: ai, tax: tax, store: store.New(pool), queue: jobs.NewQueue(pool), embed: e}
	return jobs.Registry{
		KindEmbedRole:    e.role,
		KindEmbedProfile: e.profile,
		KindParseResume:  r.parse,
	}
}

type embedder struct {
	pool *pgxpool.Pool
	ai   Embedder
}

// Embedder is the one AI call the embedding handlers make.
type Embedder interface {
	EmbedBatch(ctx context.Context, items []aiclient.EmbedItem) (aiclient.EmbedBatchResponse, error)
}

// EmbedRole embeds a role now instead of through the queue, exactly as the
// embed_role handler would. Role intake uses it so that the role it answers
// with is already embedded; when it fails, queueing KindEmbedRole does the
// same work later.
func EmbedRole(ctx context.Context, pool *pgxpool.Pool, ai Embedder, roleID string) error {
	payload, err := json.Marshal(map[string]string{"role_id": roleID})
	if err != nil {
		return err
	}
	return (&embedder{pool: pool, ai: ai}).role(ctx, jobs.Job{Kind: KindEmbedRole, Payload: payload})
}

// role embeds a role. A role with structured requirements is sent as those
// requirements, which the AI service renders to the same canonical text it
// renders a profile to, so the two vectors are comparable; a role that has
// only a description (not parsed yet) is embedded from the description. The
// store clears the embedding whenever one of the columns read here changes.
func (e *embedder) role(ctx context.Context, job jobs.Job) error {
	var p struct {
		RoleID string `json:"role_id"`
	}
	if err := json.Unmarshal(job.Payload, &p); err != nil || strings.TrimSpace(p.RoleID) == "" {
		return jobs.PayloadError(job, `{"role_id": uuid}`)
	}
	var title, description string
	var requirements, must, nice []byte
	var certs, software []string
	err := e.pool.QueryRow(ctx, `
		SELECT title, description, requirements, must_haves, nice_to_haves, required_certifications, required_software
		FROM roles WHERE id = $1`, p.RoleID).Scan(&title, &description, &requirements, &must, &nice, &certs, &software)
	if done, err := rowGone(job, err); done {
		return err
	}
	if certs == nil {
		certs = []string{} // a nil slice would be sent as NULL and never compare equal below
	}
	if software == nil {
		software = []string{}
	}
	doc := roleDocument(title, requirements, stringList(must), stringList(nice), certs, software)
	if doc == nil && strings.TrimSpace(description) == "" {
		return nil // nothing to embed; the column stays NULL
	}
	vec, model, err := e.embed(ctx, aiclient.EmbedItem{Requirements: doc}, description)
	if err != nil {
		return err
	}
	return e.write(ctx, job, `
		UPDATE roles SET embedding = $2::vector, embedding_model = $3, embedded_at = now()
		WHERE id = $1 AND title = $4 AND description = $5 AND requirements = $6::jsonb
		  AND must_haves = $7::jsonb AND nice_to_haves = $8::jsonb
		  AND required_certifications = $9 AND required_software = $10`,
		p.RoleID, vec, model, title, description, requirements, must, nice, certs, software)
}

// profile embeds a candidate profile from the same fields whose change makes
// the store clear the embedding: headline, the structured profile, and the
// certification and software lists. A profile the AI service accepts as a
// CandidateProfile is embedded as its canonical text; any other JSON is
// embedded as profileText.
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
	vec, model, err := e.embed(ctx, aiclient.EmbedItem{Profile: profileDocument(headline, profile, certs, software)}, text)
	if err != nil {
		return err
	}
	return e.write(ctx, job, `
		UPDATE candidate_profiles SET embedding = $2::vector, embedding_model = $3, embedded_at = now()
		WHERE candidate_id = $1 AND headline IS NOT DISTINCT FROM $4 AND profile = $5::jsonb
		  AND certifications = $6 AND software = $7`,
		p.CandidateID, vec, model, headline, profile, certs, software)
}

// RoleStructured reports whether a role has requirements beyond its title
// and description: a parsed requirements document, or any of the promoted
// lists. Such a role is embedded from them rather than from the raw JD.
func RoleStructured(requirements []byte, mustHaves, niceToHaves, certs, software []string) bool {
	return len(jsonObject(requirements)) > 0 || len(mustHaves)+len(niceToHaves)+len(certs)+len(software) > 0
}

// roleDocument is the RoleRequirements sent for a structured role, or nil
// for one that is not (see RoleStructured). It is the stored requirements
// with the row's own columns laid over them: the title always, and each list
// that is not empty, since the columns are what the API edits and what the
// hard filters read.
func roleDocument(title string, requirements []byte, mustHaves, niceToHaves, certs, software []string) json.RawMessage {
	if !RoleStructured(requirements, mustHaves, niceToHaves, certs, software) {
		return nil
	}
	doc := jsonObject(requirements)
	overlay(doc, "title", title)
	overlayList(doc, "must_haves", mustHaves)
	overlayList(doc, "nice_to_haves", niceToHaves)
	overlayList(doc, "required_certifications", certs)
	overlayList(doc, "required_software", software)
	out, _ := json.Marshal(doc)
	return out
}

// profileDocument is the CandidateProfile sent for a profile: the stored
// JSON with the headline and the hard-filter lists laid over it where the
// columns have a value. It is nil when the stored JSON is not an object.
func profileDocument(headline *string, profile []byte, certs, software []string) json.RawMessage {
	doc := jsonObject(profile)
	if doc == nil {
		return nil
	}
	if headline != nil && strings.TrimSpace(*headline) != "" {
		overlay(doc, "headline", strings.TrimSpace(*headline))
	}
	overlayList(doc, "certifications", certs)
	overlayList(doc, "software", software)
	out, _ := json.Marshal(doc)
	return out
}

// jsonObject decodes a JSON object one level deep; nil for anything else.
func jsonObject(raw []byte) map[string]json.RawMessage {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	return doc
}

// stringList decodes a JSONB array of strings (must_haves, nice_to_haves).
func stringList(raw []byte) []string {
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

func overlay(doc map[string]json.RawMessage, key string, value any) {
	raw, _ := json.Marshal(value)
	doc[key] = raw
}

func overlayList(doc map[string]json.RawMessage, key string, values []string) {
	if len(values) > 0 {
		overlay(doc, key, values)
	}
}

// profileText is the document embedded for a profile whose JSON is not a
// CandidateProfile. It is deterministic so re-embedding unchanged input
// gives the same vector.
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

// embed calls the AI service and formats the vector for pgvector. item is
// the structured document (its Profile or Requirements; neither set means
// there is none) and text is what to embed without one. A structured
// document the service will not take (422: not valid for its model, or it
// renders to nothing) falls back to text, so a hand-written profile still
// gets a vector. Any other 4xx means this input can never be embedded, so
// it is permanent; the rest (unreachable, timeout, 5xx) is worth a retry.
func (e *embedder) embed(ctx context.Context, item aiclient.EmbedItem, text string) (string, string, error) {
	structured := item.Profile != nil || item.Requirements != nil
	if !structured {
		item = aiclient.EmbedItem{Text: text}
	}
	resp, err := e.ai.EmbedBatch(ctx, []aiclient.EmbedItem{item})
	var rejected *aiclient.Error
	if structured && errors.As(err, &rejected) && rejected.StatusCode == http.StatusUnprocessableEntity && strings.TrimSpace(text) != "" {
		resp, err = e.ai.EmbedBatch(ctx, []aiclient.EmbedItem{{Text: text}})
	}
	if err != nil {
		if errors.Is(err, aiclient.ErrBadRequest) {
			return "", "", jobs.Permanent(err)
		}
		return "", "", err
	}
	vec := resp.Embeddings[0]
	if len(vec) != EmbeddingDim {
		return "", "", jobs.Permanent(fmt.Errorf("ai service returned a %d-dimension embedding; the columns are vector(%d)", len(vec), EmbeddingDim))
	}
	return vectorLiteral(vec), resp.Provider, nil
}

// write stores the vector, but only if what it was computed from is
// still what the row holds: the WHERE clause compares exactly the columns
// the embedding is derived from (the same ones whose change makes it
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
