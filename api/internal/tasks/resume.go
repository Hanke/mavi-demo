package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
	"github.com/jackc/pgx/v5/pgxpool"
)

type resumeParser struct {
	pool  *pgxpool.Pool
	ai    AI
	tax   *taxonomy.Taxonomy
	store *store.Store
	queue *jobs.Queue
	embed *embedder
}

// logf is the handlers' log line, written with the request id and job fields
// ctx carries (internal/reqlog); a variable so tests can silence or capture it.
var logf = func(ctx context.Context, format string, args ...any) {
	slog.InfoContext(ctx, fmt.Sprintf(format, args...))
}

// parse turns a candidate's resume text (candidates.resume_text, which the
// upload endpoint fills) into their profile: the AI service's /parse-resume
// extracts it, the hard-filter fields are promoted to columns, and the row is
// embedded. The payload names only the candidate, so the job always parses
// the text the row holds now; an upload that lands while the parser runs
// makes this attempt's result stale, and it is retried with the new text
// rather than written.
func (r *resumeParser) parse(ctx context.Context, job jobs.Job) error {
	var p struct {
		CandidateID string `json:"candidate_id"`
	}
	if err := json.Unmarshal(job.Payload, &p); err != nil || strings.TrimSpace(p.CandidateID) == "" {
		return jobs.PayloadError(job, `{"candidate_id": uuid}`)
	}
	var text string
	err := r.pool.QueryRow(ctx, `SELECT resume_text FROM candidates WHERE id = $1`, p.CandidateID).Scan(&text)
	if done, err := rowGone(job, err); done {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return jobs.Permanent(fmt.Errorf("job %d: candidate %s has no resume text to parse", job.ID, p.CandidateID))
	}

	// A 4xx means this text can never be parsed (too long, say). The rest,
	// including the 502 the service answers when the model's output does not
	// validate, is worth another attempt.
	parsed, err := r.ai.ParseResume(ctx, text)
	if err != nil {
		if errors.Is(err, aiclient.ErrBadRequest) {
			return jobs.Permanent(err)
		}
		return err
	}
	in, err := r.profileInput(parsed)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("job %d: %w", job.ID, err))
	}
	if _, _, err := r.store.UpsertParsedProfile(ctx, p.CandidateID, in, text); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("job %d: resume replaced or candidate deleted while parsing; retrying with the current text", job.ID)
		}
		return err
	}

	// The profile is stored; what is left is its vector. A failure there
	// must not send the whole job round again, since that would pay for the
	// parse twice, so the embedding is handed to its own job instead.
	if err := r.embed.profile(ctx, job); err != nil {
		qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, _, qerr := r.queue.Enqueue(qctx, jobs.EnqueueInput{Kind: KindEmbedProfile, Payload: job.Payload}); qerr != nil {
			return fmt.Errorf("profile stored but not embedded (%v), and queueing %s failed: %w", err, KindEmbedProfile, qerr)
		}
		logf(ctx, "tasks: job %d: profile for candidate %s stored; embedding failed (%v), queued %s", job.ID, p.CandidateID, err, KindEmbedProfile)
	}
	return nil
}

// profileInput maps the parser's output onto the profile row: the whole
// extraction as the JSONB document, and the hard-filter fields as columns.
// The certification and software ids are checked against the API's own
// taxonomy, as PUT …/profile checks them: an id it does not know would sit
// in a column no role can ever match, so it is an error (the two services
// are reading different versions of infra/taxonomy.json), not something to
// drop. The softer fields come from a model reading somebody else's
// document, so a value the column cannot hold is left out instead.
func (r *resumeParser) profileInput(parsed aiclient.ParsedResume) (store.ProfileInput, error) {
	p := parsed.Profile
	certs, unknownCerts := r.canonical(taxonomy.Certifications, p.Certifications)
	software, unknownSoftware := r.canonical(taxonomy.Software, p.Software)
	if len(unknownCerts)+len(unknownSoftware) > 0 {
		return store.ProfileInput{}, fmt.Errorf("the parser returned ids that are not in the taxonomy (certifications: %s; software: %s)",
			orNone(unknownCerts), orNone(unknownSoftware))
	}
	in := store.ProfileInput{
		Profile:        parsed.ProfileJSON,
		Certifications: certs,
		Software:       software,
		Availability:   string(contract.AvailabilityUnknown),
	}
	if p.Headline != nil && strings.TrimSpace(*p.Headline) != "" {
		headline := strings.TrimSpace(*p.Headline)
		in.Headline = &headline
	}
	if p.YearsExperience != nil && *p.YearsExperience >= 0 && *p.YearsExperience <= 70 {
		in.YearsExperience = p.YearsExperience
	}
	if p.Availability != nil && contract.Availability(*p.Availability).Valid() {
		in.Availability = string(*p.Availability)
	}
	if p.AvailableFrom != nil {
		from := contract.Date(p.AvailableFrom.Time)
		in.AvailableFrom = &from
	}
	if p.Timezone != nil && *p.Timezone != "" && *p.Timezone != "Local" {
		if _, err := time.LoadLocation(*p.Timezone); err == nil {
			in.Timezone = p.Timezone
		}
	}
	return in, nil
}

// canonical splits the parser's ids into the ones the taxonomy has, in
// order and without repeats, and the ones it does not.
func (r *resumeParser) canonical(kind taxonomy.Kind, values []string) (ids, unknown []string) {
	seen := map[string]bool{}
	for _, v := range values {
		switch {
		case !r.tax.IsCanonical(kind, v):
			unknown = append(unknown, v)
		case !seen[v]:
			seen[v] = true
			ids = append(ids, v)
		}
	}
	return ids, unknown
}

func orNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}
