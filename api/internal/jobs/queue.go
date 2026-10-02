// Package jobs is the Postgres-backed background queue: the jobs table from
// infra/db/migrations/0003_jobs.up.sql, a Queue the API uses to enqueue and
// read jobs, and a Worker that claims them with FOR UPDATE SKIP LOCKED and
// runs the Handler registered for their kind.
//
// There is no broker. A job is a row; claiming it is an UPDATE that locks the
// row, so two workers can never take the same one. A failed attempt goes back
// to 'queued' with run_at pushed out by a backoff until max_attempts is spent,
// when the row lands in 'failed' with the error recorded in last_error.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job is the API's job schema (api/openapi.yaml), generated into
// internal/contract, so what the queue scans is what GET /jobs/{id} returns.
type Job = contract.Job

// Status values, from the contract's JobStatus enum.
const (
	StatusQueued    = contract.JobStatusQueued
	StatusRunning   = contract.JobStatusRunning
	StatusSucceeded = contract.JobStatusSucceeded
	StatusFailed    = contract.JobStatusFailed
)

// DefaultMaxAttempts is used when EnqueueInput.MaxAttempts is zero; it
// matches the column default.
const DefaultMaxAttempts = 3

// Handler runs one job. Returning nil marks the job succeeded. Any other
// error schedules a retry, or fails the job once max_attempts is spent; wrap
// it with Permanent to fail the job at once. A handler must be safe to run
// more than once: a worker that dies mid-job leaves the row 'running' until
// the lock timeout, after which another worker runs it again.
type Handler func(ctx context.Context, job Job) error

// Registry maps a job kind to its handler. The API only enqueues kinds in
// the registry, and a worker only claims kinds it can run.
type Registry map[string]Handler

// Kinds lists the registered kinds, sorted.
func (r Registry) Kinds() []string {
	out := make([]string, 0, len(r))
	for k := range r {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ErrPermanent marks a failure that retrying cannot fix (a malformed
// payload, a row that no longer exists in a way the handler cannot treat as
// done). Test with errors.Is; create with Permanent.
var ErrPermanent = errors.New("permanent failure")

// Permanent wraps err so the worker fails the job without retrying.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

type permanentError struct{ err error }

func (e *permanentError) Error() string        { return e.err.Error() }
func (e *permanentError) Unwrap() error        { return e.err }
func (e *permanentError) Is(target error) bool { return target == ErrPermanent }

// Queue enqueues and reads jobs.
type Queue struct {
	pool *pgxpool.Pool
}

func NewQueue(pool *pgxpool.Pool) *Queue { return &Queue{pool: pool} }

// EnqueueInput describes a job to add.
type EnqueueInput struct {
	Kind        string
	Payload     json.RawMessage // a JSON object; nil means {}
	Priority    int             // higher runs first
	RunAt       time.Time       // not before; zero means now
	MaxAttempts int             // zero means DefaultMaxAttempts
}

const jobCols = `id, kind, payload, status, priority, run_at, attempts, max_attempts, last_error, locked_by, locked_at,
	finished_at, created_at, updated_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Kind, &j.Payload, &j.Status, &j.Priority, &j.RunAt, &j.Attempts, &j.MaxAttempts,
		&j.LastError, &j.Worker, &j.StartedAt, &j.FinishedAt, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, store.ErrNotFound
	}
	if j.Payload == nil {
		j.Payload = json.RawMessage(`{}`)
	}
	return j, err
}

// Enqueue adds a job and returns it as stored, with created = true. If an
// identical job (same kind and payload) is already queued, nothing is added
// and that job is returned with created = false: saving a role five times
// while its embedding is still waiting must not queue five embeddings. A job
// that is running or finished does not count, so re-enqueueing after that
// creates a new one. The rule is the partial unique index
// jobs_queued_dedupe_idx (migration 0005).
func (q *Queue) Enqueue(ctx context.Context, in EnqueueInput) (job Job, created bool, err error) {
	if strings.TrimSpace(in.Kind) == "" {
		return Job{}, false, errors.New("enqueue: kind is required")
	}
	if in.Payload == nil {
		in.Payload = json.RawMessage(`{}`)
	}
	if in.MaxAttempts <= 0 {
		in.MaxAttempts = DefaultMaxAttempts
	}
	var runAt *time.Time
	if !in.RunAt.IsZero() {
		runAt = &in.RunAt
	}
	// The existing job can be claimed between the INSERT skipping it and the
	// SELECT looking for it; then the INSERT is simply tried again.
	for attempt := 0; attempt < 3; attempt++ {
		job, err = scanJob(q.pool.QueryRow(ctx, `
			INSERT INTO jobs (kind, payload, priority, run_at, max_attempts)
			VALUES ($1, $2, $3, COALESCE($4, now()), $5)
			ON CONFLICT (kind, payload) WHERE status = 'queued' DO NOTHING
			RETURNING `+jobCols, in.Kind, in.Payload, in.Priority, runAt, in.MaxAttempts))
		if err == nil {
			return job, true, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return Job{}, false, err
		}
		job, err = scanJob(q.pool.QueryRow(ctx, `
			SELECT `+jobCols+` FROM jobs WHERE kind = $1 AND payload = $2 AND status = 'queued'`, in.Kind, in.Payload))
		if err == nil {
			return job, false, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return Job{}, false, err
		}
	}
	return Job{}, false, errors.New("enqueue: an identical job kept being claimed between insert and lookup")
}

// Get returns one job, or store.ErrNotFound.
func (q *Queue) Get(ctx context.Context, id int64) (Job, error) {
	return scanJob(q.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = $1`, id))
}

// Latest returns the newest job of a kind with exactly this payload, whatever
// its status, or store.ErrNotFound. A handler whose payload names one row
// (a candidate, a role) has at most one queued job for it, so this is "how
// is the work for that row going".
func (q *Queue) Latest(ctx context.Context, kind string, payload json.RawMessage) (Job, error) {
	return scanJob(q.pool.QueryRow(ctx, `
		SELECT `+jobCols+` FROM jobs WHERE kind = $1 AND payload = $2 ORDER BY id DESC LIMIT 1`, kind, payload))
}

// Filter narrows List; empty fields match anything.
type Filter struct {
	Status string
	Kind   string
}

// List returns jobs newest first.
func (q *Queue) List(ctx context.Context, f Filter, p store.Page) ([]Job, error) {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Limit > 200 {
		p.Limit = 200
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	rows, err := q.pool.Query(ctx, `
		SELECT `+jobCols+` FROM jobs
		WHERE ($1 = '' OR status = $1) AND ($2 = '' OR kind = $2)
		ORDER BY created_at DESC, id DESC
		LIMIT $3 OFFSET $4`, f.Status, f.Kind, p.Limit, p.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// IsInvalidText reports a Postgres invalid_text_representation error, e.g.
// a payload id that is not a UUID. Handlers use it to fail permanently
// instead of retrying a query that can never succeed.
func IsInvalidText(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// PayloadError is the Permanent error for a payload the handler cannot use.
func PayloadError(job Job, want string) error {
	return Permanent(fmt.Errorf("job %d (%s): payload %s does not look like %s", job.ID, job.Kind, string(job.Payload), want))
}
