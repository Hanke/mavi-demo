package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/colehanke/mavi-demo/api/internal/reqlog"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkerConfig tunes a Worker. Zero values take the defaults noted.
type WorkerConfig struct {
	// ID names this worker in jobs.locked_by; default "<hostname>-<pid>".
	ID string
	// Concurrency is how many jobs run at once; default 1.
	Concurrency int
	// PollInterval is how long an idle loop sleeps before looking again;
	// default 1s. Jobs are claimed back to back while there are any.
	PollInterval time.Duration
	// LockTimeout is how long a job may stay 'running' before another worker
	// assumes its owner died and reclaims it; default 5m. It must be longer
	// than the slowest handler, or a live job gets run twice.
	LockTimeout time.Duration
	// ShutdownGrace is how long in-flight handlers keep running after Run's
	// context is cancelled before their own context is cancelled; default 5s.
	// A handler that returns because of that cancellation hands its job back
	// to the queue without spending an attempt.
	ShutdownGrace time.Duration
	// Backoff returns how long to wait before retrying after the given number
	// of failed attempts (1 for the first); default DefaultBackoff.
	Backoff func(attempts int) time.Duration
	// Logger receives one line per claim, completion, retry, failure and
	// reclaim; default slog.Default().
	Logger *slog.Logger
}

// DefaultBackoff waits 5s after the first failure and quadruples each time,
// capped at 10 minutes: 5s, 20s, 80s, 5m20s, 10m, 10m, …
func DefaultBackoff(attempts int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < attempts; i++ {
		d *= 4
		if d >= 10*time.Minute {
			return 10 * time.Minute
		}
	}
	return d
}

// Worker claims queued jobs and runs their handlers. Several workers, in one
// process or many, can share the same table.
type Worker struct {
	pool  *pgxpool.Pool
	reg   Registry
	kinds []string
	cfg   WorkerConfig
}

func NewWorker(pool *pgxpool.Pool, reg Registry, cfg WorkerConfig) *Worker {
	if cfg.ID == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "worker"
		}
		cfg.ID = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.LockTimeout <= 0 {
		cfg.LockTimeout = 5 * time.Minute
	}
	if cfg.ShutdownGrace <= 0 {
		cfg.ShutdownGrace = 5 * time.Second
	}
	if cfg.Backoff == nil {
		cfg.Backoff = DefaultBackoff
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Worker{pool: pool, reg: reg, kinds: reg.Kinds(), cfg: cfg}
}

// ID is the worker's name as written to jobs.locked_by.
func (w *Worker) ID() string { return w.cfg.ID }

// Run claims and runs jobs until ctx is cancelled, then waits for in-flight
// handlers (up to ShutdownGrace) and returns nil. It always returns nil on
// shutdown; transient database errors are logged and retried, not returned.
func (w *Worker) Run(ctx context.Context) error {
	if len(w.kinds) == 0 {
		return errors.New("jobs: worker has no handlers registered")
	}

	// Handlers get a context that outlives ctx by ShutdownGrace, so a
	// SIGTERM lets a short job finish rather than throwing its work away.
	jobCtx, cancelJobs := context.WithCancel(context.Background())
	defer cancelJobs()
	go func() {
		<-ctx.Done()
		select {
		case <-time.After(w.cfg.ShutdownGrace):
		case <-jobCtx.Done():
		}
		cancelJobs()
	}()

	var wg sync.WaitGroup
	for i := 0; i < w.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx, jobCtx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.sweep(ctx)
	}()
	wg.Wait()
	return nil
}

// loop is one claim-run cycle: take the next job, run it, repeat; sleep only
// when the queue is empty.
func (w *Worker) loop(ctx, jobCtx context.Context) {
	for ctx.Err() == nil {
		job, ok, err := w.claim()
		switch {
		case err != nil:
			if ctx.Err() == nil {
				w.cfg.Logger.Error("jobs: claim failed", "worker", w.cfg.ID, "error", err.Error())
			}
			w.wait(ctx)
		case ok:
			// Always run a claimed job, even if ctx was cancelled while the
			// claim was in flight: the claim is committed, and jobCtx stays
			// live for ShutdownGrace so the job either finishes or is
			// released. Returning here would strand it in 'running'.
			w.run(jobCtx, job)
		default:
			w.wait(ctx)
		}
	}
}

func (w *Worker) wait(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(w.cfg.PollInterval):
	}
}

// bookkeepingTimeout bounds the claim and finishing statements. They run on
// their own context rather than the worker's, so a shutdown cannot cancel a
// statement Postgres has already committed and leave the row out of step
// with what this process believes.
const bookkeepingTimeout = 10 * time.Second

// claim takes the next runnable job of a kind this worker can handle. The
// subquery locks the candidate row with SKIP LOCKED, so concurrent workers
// each get a different row and never block on each other; the UPDATE that
// marks it running and counts the attempt commits in the same statement.
func (w *Worker) claim() (Job, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bookkeepingTimeout)
	defer cancel()
	job, err := scanJob(w.pool.QueryRow(ctx, `
		UPDATE jobs SET status = 'running', locked_at = now(), locked_by = $1, attempts = attempts + 1
		WHERE id = (
			SELECT id FROM jobs
			WHERE status = 'queued' AND run_at <= now() AND kind = ANY($2)
			ORDER BY priority DESC, run_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1)
		RETURNING `+jobCols, w.cfg.ID, w.kinds))
	if errors.Is(err, store.ErrNotFound) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return job, true, nil
}

// run executes one claimed job and records the outcome. The bookkeeping
// writes use their own short context so a cancelled jobCtx cannot leave the
// row stuck in 'running'.
//
// Each attempt runs under a request id of its own (internal/reqlog): it is on
// every line logged here and by the handler, with the job's id, kind and
// attempt, and on the AI service's lines for the calls the handler makes.
func (w *Worker) run(jobCtx context.Context, job Job) {
	start := time.Now()
	jobCtx = reqlog.With(reqlog.WithID(jobCtx, reqlog.NewID()),
		"worker", w.cfg.ID, "job_id", job.ID, "job_kind", job.Kind, "attempt", job.Attempts)
	log := w.cfg.Logger
	log.InfoContext(jobCtx, "job started", "max_attempts", job.MaxAttempts)

	err := w.invoke(jobCtx, job)

	ctx, cancel := context.WithTimeout(context.Background(), bookkeepingTimeout)
	defer cancel()
	elapsed := slog.Int64("duration_ms", time.Since(start).Milliseconds())
	switch {
	case err == nil:
		if err := w.complete(ctx, job); err != nil {
			log.ErrorContext(jobCtx, "job: record success failed", "error", err.Error())
			return
		}
		log.InfoContext(jobCtx, "job succeeded", elapsed)
	case jobCtx.Err() != nil && !errors.Is(err, ErrPermanent):
		// Shutting down: the handler was interrupted, not wrong. Hand the job
		// back without charging an attempt.
		if err := w.release(ctx, job); err != nil {
			log.ErrorContext(jobCtx, "job: release on shutdown failed", "error", err.Error())
			return
		}
		log.InfoContext(jobCtx, "job released on shutdown", elapsed)
	default:
		status, ferr := w.fail(ctx, job, err)
		if ferr != nil {
			log.ErrorContext(jobCtx, "job: record failure failed", "error", ferr.Error())
			return
		}
		// status is where the job went: back to queued for a retry, or failed.
		log.WarnContext(jobCtx, "job attempt failed", elapsed, "max_attempts", job.MaxAttempts, "status", status, "error", err.Error())
	}
}

// invoke runs the handler, turning a panic into an ordinary failure so one
// bad job cannot take the worker down.
func (w *Worker) invoke(ctx context.Context, job Job) (err error) {
	h, ok := w.reg[job.Kind]
	if !ok {
		return Permanent(fmt.Errorf("no handler for kind %q", job.Kind))
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, job)
}

// The finishing statements all match on the exact claim: still 'running',
// locked by this worker, and on the same attempt number the claim returned.
// A job the sweeper reclaimed and someone re-claimed (possibly this very
// worker, on a later attempt) is therefore never overwritten by the stale,
// slow attempt when it finally returns.
const ownedRunning = ` WHERE id = $1 AND status = 'running' AND locked_by = $2 AND attempts = $3`

// queuedTwin is true, in an UPDATE of jobs, for a row that cannot go back to
// 'queued': an identical job (same kind and payload) is queued already, and
// jobs_queued_dedupe_idx allows one. That happens when the same work was
// enqueued again while this job ran (a role saved twice, a resume uploaded
// again). The queued one does the work, so this one is finished as failed
// with a note rather than requeued, which would violate the index and leave
// the row stuck in 'running'.
const queuedTwin = `EXISTS (SELECT 1 FROM jobs q WHERE q.kind = jobs.kind AND q.payload = jobs.payload AND q.status = 'queued' AND q.id <> jobs.id)`

const twinNote = `not requeued: an identical job is already queued`

// errNotOwned is returned when a finishing statement matched no row.
var errNotOwned = errors.New("job is no longer owned by this claim (lock timed out and it was reclaimed?)")

func (w *Worker) complete(ctx context.Context, job Job) error {
	tag, err := w.pool.Exec(ctx, `
		UPDATE jobs SET status = 'succeeded', finished_at = now(), last_error = NULL`+ownedRunning,
		job.ID, w.cfg.ID, job.Attempts)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotOwned
	}
	return nil
}

// maxErrorLen bounds last_error; a stack trace or a large response body
// does not need to live in the queue table.
const maxErrorLen = 4 << 10

// fail records a failed attempt: back to 'queued' after a backoff while
// attempts remain, otherwise 'failed'. Both branches keep the error. A job
// with attempts left whose identical twin is queued is failed too (queuedTwin).
func (w *Worker) fail(ctx context.Context, job Job, cause error) (string, error) {
	msg := truncate(cause.Error(), maxErrorLen)
	permanent := errors.Is(cause, ErrPermanent)
	delay := w.cfg.Backoff(job.Attempts)
	var status string
	err := w.pool.QueryRow(ctx, `
		UPDATE jobs SET
			status      = CASE WHEN $4 OR attempts >= max_attempts OR `+queuedTwin+` THEN 'failed' ELSE 'queued' END,
			run_at      = CASE WHEN $4 OR attempts >= max_attempts OR `+queuedTwin+` THEN run_at ELSE now() + make_interval(secs => $5) END,
			finished_at = CASE WHEN $4 OR attempts >= max_attempts OR `+queuedTwin+` THEN now() ELSE NULL END,
			last_error  = $6 || CASE WHEN NOT ($4 OR attempts >= max_attempts) AND `+queuedTwin+` THEN $7 ELSE '' END`+ownedRunning+`
		RETURNING status`, job.ID, w.cfg.ID, job.Attempts, permanent, delay.Seconds(), msg, "; "+twinNote).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotOwned
	}
	return status, err
}

// truncate cuts s to at most n bytes on a rune boundary, after replacing any
// invalid UTF-8, so the result is always a string Postgres will accept.
func truncate(s string, n int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// release hands an interrupted job back: queued, runnable now, and with the
// attempt the claim counted given back. locked_by / locked_at stay as a
// record of who held it last; ownership is decided by status and attempts.
// When an identical job is queued already, that one takes its place and this
// one is finished instead (queuedTwin).
func (w *Worker) release(ctx context.Context, job Job) error {
	tag, err := w.pool.Exec(ctx, `
		UPDATE jobs SET
			status      = CASE WHEN `+queuedTwin+` THEN 'failed' ELSE 'queued' END,
			run_at      = CASE WHEN `+queuedTwin+` THEN run_at ELSE now() END,
			finished_at = CASE WHEN `+queuedTwin+` THEN now() ELSE NULL END,
			last_error  = CASE WHEN `+queuedTwin+` THEN $4 ELSE last_error END,
			attempts    = attempts - 1`+ownedRunning,
		job.ID, w.cfg.ID, job.Attempts, "interrupted by shutdown; "+twinNote)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotOwned
	}
	return nil
}

// sweep periodically reclaims jobs whose worker went away.
func (w *Worker) sweep(ctx context.Context) {
	every := w.cfg.LockTimeout / 2
	if every < time.Second {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := w.Reclaim(ctx, w.cfg.LockTimeout)
			if err != nil && ctx.Err() == nil {
				w.cfg.Logger.Error("jobs: reclaim stale jobs failed", "worker", w.cfg.ID, "error", err.Error())
			}
			if n > 0 {
				w.cfg.Logger.Warn("jobs: reclaimed jobs whose lock expired", "worker", w.cfg.ID, "jobs", n)
			}
		}
	}
}

// Reclaim returns every job that has been 'running' longer than olderThan to
// the queue, or fails it when its attempts are spent (the claim already
// counted the attempt). locked_by / locked_at are kept so the row still says
// which worker held it. It reports how many rows it touched. Run calls it
// on a timer; it is exported for tests and one-off repair.
//
// Only one job per (kind, payload) may be queued, so a stale job is failed
// rather than requeued when an identical one is queued already (queuedTwin)
// or is being requeued by this same statement (the oldest stale one with
// attempts left is). One such row must not abort the statement, which would
// leave every other stale job running.
func (w *Worker) Reclaim(ctx context.Context, olderThan time.Duration) (int64, error) {
	const spent = `(attempts >= max_attempts OR ` + queuedTwin + ` OR EXISTS (
			SELECT 1 FROM jobs o
			WHERE o.kind = jobs.kind AND o.payload = jobs.payload AND o.id < jobs.id AND o.status = 'running'
			  AND o.locked_at < now() - make_interval(secs => $2) AND o.attempts < o.max_attempts))`
	tag, err := w.pool.Exec(ctx, `
		UPDATE jobs SET
			status      = CASE WHEN `+spent+` THEN 'failed' ELSE 'queued' END,
			finished_at = CASE WHEN `+spent+` THEN now() ELSE NULL END,
			last_error  = 'lock expired: worker ' || COALESCE(locked_by, '?') || ' did not finish within ' || $1::text
			              || CASE WHEN attempts < max_attempts AND `+spent+` THEN $3 ELSE '' END
		WHERE status = 'running' AND locked_at < now() - make_interval(secs => $2)`,
		olderThan.String(), olderThan.Seconds(), "; "+twinNote)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
