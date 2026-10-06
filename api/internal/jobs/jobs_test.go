package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/colehanke/mavi-demo/api/internal/dbtest"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests run against a throwaway database (see dbtest) and skip when
// TEST_DATABASE_URL is unset. They cover the ticket's acceptance criteria:
// an enqueued job is picked up and completed; a failing job retries and then
// lands in 'failed' with the error recorded; two workers never process the
// same job.

var quiet = slog.New(slog.DiscardHandler)

// fast is a worker config tuned for tests: tight polling, no retry delay.
func fast(id string, concurrency int) WorkerConfig {
	return WorkerConfig{
		ID:            id,
		Concurrency:   concurrency,
		PollInterval:  10 * time.Millisecond,
		ShutdownGrace: 100 * time.Millisecond,
		Backoff:       func(int) time.Duration { return 0 },
		Logger:        quiet,
	}
}

// start runs a worker until the test ends (or stop is called) and returns
// stop, which blocks until Run has returned.
func start(t *testing.T, pool *pgxpool.Pool, reg Registry, cfg WorkerConfig) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := NewWorker(pool, reg, cfg).Run(ctx); err != nil {
			t.Errorf("worker %s: %v", cfg.ID, err)
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return stop
}

// waitFor polls cond until it is true or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func get(t *testing.T, q *Queue, id int64) Job {
	t.Helper()
	j, err := q.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get job %d: %v", id, err)
	}
	return j
}

func status(t *testing.T, q *Queue, id int64) string {
	t.Helper()
	return string(get(t, q, id).Status)
}

func TestEnqueuedJobIsPickedUpAndCompleted(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	ctx := context.Background()

	var mu sync.Mutex
	var seen []string
	reg := Registry{"echo": func(_ context.Context, job Job) error {
		var p struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return err
		}
		mu.Lock()
		seen = append(seen, p.Msg)
		mu.Unlock()
		return nil
	}}

	job, _, err := q.Enqueue(ctx, EnqueueInput{Kind: "echo", Payload: json.RawMessage(`{"msg":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusQueued || job.Attempts != 0 || job.MaxAttempts != DefaultMaxAttempts || job.Worker != nil {
		t.Fatalf("fresh job: %+v", job)
	}

	start(t, pool, reg, fast("w1", 1))
	waitFor(t, "job to succeed", func() bool { return status(t, q, job.ID) == "succeeded" })

	got := get(t, q, job.ID)
	if got.Attempts != 1 || got.FinishedAt == nil || got.StartedAt == nil || got.LastError != nil {
		t.Fatalf("succeeded job: %+v", got)
	}
	if got.Worker == nil || *got.Worker != "w1" {
		t.Fatalf("worker = %v, want w1", got.Worker)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "hello" {
		t.Fatalf("handler saw %v, want [hello]", seen)
	}
}

func TestFailedJobRetriesThenFailsWithError(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	ctx := context.Background()

	var calls int32
	var mu sync.Mutex
	reg := Registry{"flaky": func(_ context.Context, job Job) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		return fmt.Errorf("boom %d (attempt %d)", n, job.Attempts)
	}}

	job, _, err := q.Enqueue(ctx, EnqueueInput{Kind: "flaky", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	start(t, pool, reg, fast("w1", 1))
	waitFor(t, "job to fail", func() bool { return status(t, q, job.ID) == "failed" })

	got := get(t, q, job.ID)
	if got.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", got.Attempts)
	}
	if got.LastError == nil || *got.LastError != "boom 3 (attempt 3)" {
		t.Fatalf("last_error = %v, want the third attempt's error", got.LastError)
	}
	if got.FinishedAt == nil {
		t.Fatal("failed job should have finished_at")
	}
	// Give the worker a moment to prove it does not touch a failed job again.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("handler ran %d times, want exactly 3", calls)
	}
}

func TestRetryWaitsForBackoff(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	ctx := context.Background()

	reg := Registry{"flaky": func(context.Context, Job) error { return errors.New("nope") }}
	job, _, err := q.Enqueue(ctx, EnqueueInput{Kind: "flaky", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	cfg := fast("w1", 1)
	cfg.Backoff = func(int) time.Duration { return time.Hour }
	start(t, pool, reg, cfg)

	// The claim counts the attempt before the handler runs, so the count alone
	// does not say the failure has been recorded yet.
	waitFor(t, "first attempt to be recorded", func() bool {
		got := get(t, q, job.ID)
		return got.Attempts == 1 && got.Status != StatusRunning
	})
	got := get(t, q, job.ID)
	if got.Status != StatusQueued {
		t.Fatalf("status after first failure = %s, want queued", got.Status)
	}
	if got.LastError == nil || *got.LastError != "nope" {
		t.Fatalf("last_error = %v, want nope", got.LastError)
	}
	if until := time.Until(got.RunAt); until < 50*time.Minute {
		t.Fatalf("run_at only %s away; the backoff should have pushed it out an hour", until)
	}
	time.Sleep(50 * time.Millisecond)
	if get(t, q, job.ID).Attempts != 1 {
		t.Fatal("job was retried before its run_at")
	}
}

func TestPermanentErrorSkipsRetries(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	ctx := context.Background()

	reg := Registry{"bad": func(context.Context, Job) error { return Permanent(errors.New("payload is nonsense")) }}
	job, _, err := q.Enqueue(ctx, EnqueueInput{Kind: "bad", MaxAttempts: 5})
	if err != nil {
		t.Fatal(err)
	}
	start(t, pool, reg, fast("w1", 1))
	waitFor(t, "job to fail", func() bool { return status(t, q, job.ID) == "failed" })
	got := get(t, q, job.ID)
	if got.Attempts != 1 || got.LastError == nil || *got.LastError != "payload is nonsense" {
		t.Fatalf("permanent failure: %+v", got)
	}
}

func TestPanicIsRecordedAsFailure(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	reg := Registry{"panicky": func(context.Context, Job) error { panic("oh no") }}
	job, _, err := q.Enqueue(context.Background(), EnqueueInput{Kind: "panicky", MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	start(t, pool, reg, fast("w1", 1))
	waitFor(t, "job to fail", func() bool { return status(t, q, job.ID) == "failed" })
	if got := get(t, q, job.ID); got.LastError == nil || !strings.Contains(*got.LastError, "panic: oh no") {
		t.Fatalf("last_error = %v", got.LastError)
	}
}

func TestTwoWorkersNeverProcessTheSameJob(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	ctx := context.Background()

	const n = 60
	var mu sync.Mutex
	runs := map[int64][]string{} // job id -> worker ids that ran it
	handler := func(worker string) Handler {
		return func(_ context.Context, job Job) error {
			mu.Lock()
			runs[job.ID] = append(runs[job.ID], worker)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond) // long enough for claims to overlap
			return nil
		}
	}

	for i := 0; i < n; i++ {
		if _, _, err := q.Enqueue(ctx, EnqueueInput{Kind: "work", Payload: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))}); err != nil {
			t.Fatal(err)
		}
	}
	// Two workers, each running two jobs at a time, from the same table.
	start(t, pool, Registry{"work": handler("a")}, fast("a", 2))
	start(t, pool, Registry{"work": handler("b")}, fast("b", 2))

	waitFor(t, "all jobs to succeed", func() bool {
		var left int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE status <> 'succeeded'`).Scan(&left); err != nil {
			t.Fatal(err)
		}
		return left == 0
	})

	mu.Lock()
	defer mu.Unlock()
	if len(runs) != n {
		t.Fatalf("%d distinct jobs ran, want %d", len(runs), n)
	}
	byWorker := map[string]int{}
	for id, workers := range runs {
		if len(workers) != 1 {
			t.Errorf("job %d ran %d times by %v", id, len(workers), workers)
		}
		byWorker[workers[0]]++
	}
	if byWorker["a"] == 0 || byWorker["b"] == 0 {
		t.Fatalf("both workers should have taken jobs: %v", byWorker)
	}
	// The row agrees with the handler about who ran it, and ran once.
	jobs, err := q.List(ctx, Filter{}, store.Page{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Attempts != 1 {
			t.Errorf("job %d attempts = %d, want 1", j.ID, j.Attempts)
		}
		if j.Worker == nil || runs[j.ID][0] != *j.Worker {
			t.Errorf("job %d: locked_by = %v, handler ran in %v", j.ID, j.Worker, runs[j.ID])
		}
	}
}

func TestClaimHonoursRunAtPriorityAndKind(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	ctx := context.Background()

	var mu sync.Mutex
	var order []string
	reg := Registry{"known": func(_ context.Context, job Job) error {
		mu.Lock()
		order = append(order, string(job.Payload))
		mu.Unlock()
		return nil
	}}

	// Not runnable by this worker: an unregistered kind and a job in the future.
	other, _, _ := q.Enqueue(ctx, EnqueueInput{Kind: "unknown"})
	later, _, _ := q.Enqueue(ctx, EnqueueInput{Kind: "known", RunAt: time.Now().Add(time.Hour)})
	// Runnable, enqueued low priority first: the high-priority one must run first.
	q.Enqueue(ctx, EnqueueInput{Kind: "known", Payload: json.RawMessage(`"low"`), Priority: 0})
	q.Enqueue(ctx, EnqueueInput{Kind: "known", Payload: json.RawMessage(`"high"`), Priority: 10})

	start(t, pool, reg, fast("w1", 1))
	waitFor(t, "both runnable jobs", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 2 })
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	if order[0] != `"high"` || order[1] != `"low"` {
		t.Fatalf("ran in order %v, want high before low", order)
	}
	mu.Unlock()
	if s := status(t, q, other.ID); s != "queued" {
		t.Fatalf("unregistered kind should stay queued, got %s", s)
	}
	if s := status(t, q, later.ID); s != "queued" {
		t.Fatalf("future job should stay queued, got %s", s)
	}
}

func TestShutdownReleasesInFlightJobWithoutChargingAnAttempt(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	ctx := context.Background()

	started := make(chan struct{})
	reg := Registry{"slow": func(ctx context.Context, _ Job) error {
		close(started)
		<-ctx.Done() // runs until the worker's grace period cancels it
		return ctx.Err()
	}}
	job, _, err := q.Enqueue(ctx, EnqueueInput{Kind: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	stop := start(t, pool, reg, fast("w1", 1))
	<-started
	if s := status(t, q, job.ID); s != "running" {
		t.Fatalf("status while handler runs = %s", s)
	}
	stop()

	got := get(t, q, job.ID)
	if got.Status != StatusQueued || got.Attempts != 0 || got.LastError != nil {
		t.Fatalf("after shutdown the job should be back in the queue with its attempt refunded: %+v", got)
	}
	if got.Worker == nil || *got.Worker != "w1" || got.StartedAt == nil {
		t.Fatalf("the row should still say who held it last: %+v", got)
	}
	// It is runnable again by anyone, straight away.
	start(t, pool, Registry{"slow": func(context.Context, Job) error { return nil }}, fast("w2", 1))
	waitFor(t, "job to be re-run", func() bool { return status(t, q, job.ID) == "succeeded" })
	if got := get(t, q, job.ID); got.Attempts != 1 || *got.Worker != "w2" {
		t.Fatalf("re-run: %+v", got)
	}
}

func TestReclaimReturnsStaleRunningJobs(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	ctx := context.Background()

	// A worker died holding two jobs: one with attempts to spare, one without.
	var retry, spent int64
	if err := pool.QueryRow(ctx, `INSERT INTO jobs (kind, status, attempts, max_attempts, locked_by, locked_at)
		VALUES ('x', 'running', 1, 3, 'dead', now() - interval '1 hour') RETURNING id`).Scan(&retry); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO jobs (kind, status, attempts, max_attempts, locked_by, locked_at)
		VALUES ('x', 'running', 3, 3, 'dead', now() - interval '1 hour') RETURNING id`).Scan(&spent); err != nil {
		t.Fatal(err)
	}
	// A live one, claimed just now, must be left alone.
	fresh, _, err := q.Enqueue(ctx, EnqueueInput{Kind: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'running', attempts = 1, locked_by = 'alive', locked_at = now() WHERE id = $1`, fresh.ID); err != nil {
		t.Fatal(err)
	}

	w := NewWorker(pool, Registry{"x": func(context.Context, Job) error { return nil }}, WorkerConfig{ID: "sweeper", Logger: quiet})
	n, err := w.Reclaim(ctx, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("reclaimed %d, want 2", n)
	}
	if got := get(t, q, retry); got.Status != StatusQueued || got.Worker == nil || *got.Worker != "dead" || got.LastError == nil || !strings.Contains(*got.LastError, "worker dead") {
		t.Fatalf("stale job with attempts left: %+v", got)
	}
	if got := get(t, q, spent); got.Status != StatusFailed || got.FinishedAt == nil {
		t.Fatalf("stale job out of attempts: %+v", got)
	}
	if got := get(t, q, fresh.ID); got.Status != StatusRunning || got.Worker == nil || *got.Worker != "alive" {
		t.Fatalf("live job was touched: %+v", got)
	}
}

func TestQueueGetAndList(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	ctx := context.Background()

	if _, err := q.Get(ctx, 12345); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing job: err = %v, want ErrNotFound", err)
	}
	if _, _, err := q.Enqueue(ctx, EnqueueInput{Kind: ""}); err == nil {
		t.Fatal("empty kind should be rejected")
	}
	a, _, _ := q.Enqueue(ctx, EnqueueInput{Kind: "a"})
	b, _, _ := q.Enqueue(ctx, EnqueueInput{Kind: "b"})
	if string(a.Payload) != "{}" {
		t.Fatalf("nil payload should be stored as {}, got %s", a.Payload)
	}

	all, err := q.List(ctx, Filter{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != b.ID || all[1].ID != a.ID {
		t.Fatalf("list should be newest first: %+v", all)
	}
	onlyA, _ := q.List(ctx, Filter{Kind: "a"}, store.Page{})
	if len(onlyA) != 1 || onlyA[0].ID != a.ID {
		t.Fatalf("kind filter: %+v", onlyA)
	}
	none, _ := q.List(ctx, Filter{Status: "failed"}, store.Page{})
	if len(none) != 0 {
		t.Fatalf("status filter: %+v", none)
	}
}

func TestEnqueueDedupesQueuedJobs(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	ctx := context.Background()

	first, created, err := q.Enqueue(ctx, EnqueueInput{Kind: "embed", Payload: json.RawMessage(`{"role_id":"r1"}`)})
	if err != nil || !created {
		t.Fatalf("first enqueue: created=%v err=%v", created, err)
	}
	// Same kind and payload (even spelled differently) while it waits: reused.
	again, created, err := q.Enqueue(ctx, EnqueueInput{Kind: "embed", Payload: json.RawMessage(`{ "role_id" : "r1" }`), Priority: 9})
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("duplicate should return the waiting job: created=%v id=%d want %d err=%v", created, again.ID, first.ID, err)
	}
	// A different payload or kind is a different job.
	if other, created, _ := q.Enqueue(ctx, EnqueueInput{Kind: "embed", Payload: json.RawMessage(`{"role_id":"r2"}`)}); !created || other.ID == first.ID {
		t.Fatalf("different payload should be a new job: created=%v", created)
	}
	if other, created, _ := q.Enqueue(ctx, EnqueueInput{Kind: "other", Payload: json.RawMessage(`{"role_id":"r1"}`)}); !created || other.ID == first.ID {
		t.Fatalf("different kind should be a new job: created=%v", created)
	}
	// Once the first is no longer queued, the same payload can be queued again.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'succeeded', finished_at = now() WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if next, created, _ := q.Enqueue(ctx, EnqueueInput{Kind: "embed", Payload: json.RawMessage(`{"role_id":"r1"}`)}); !created || next.ID == first.ID {
		t.Fatalf("after the first finished, a new job should be created: created=%v", created)
	}
}

// A finishing statement matches only the claim that produced it. Here a
// stale attempt 1 tries to finish a row that is now on attempt 2.
func TestFinishRequiresTheSameClaim(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	ctx := context.Background()
	w := NewWorker(pool, Registry{"x": func(context.Context, Job) error { return nil }}, WorkerConfig{ID: "w1", Logger: quiet})

	job, _, err := q.Enqueue(ctx, EnqueueInput{Kind: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'running', attempts = 2, locked_by = 'w1', locked_at = now() WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	stale := job
	stale.Attempts = 1
	if err := w.complete(ctx, stale); !errors.Is(err, errNotOwned) {
		t.Fatalf("complete from a stale claim: err = %v, want errNotOwned", err)
	}
	if _, err := w.fail(ctx, stale, errors.New("late")); !errors.Is(err, errNotOwned) {
		t.Fatalf("fail from a stale claim: err = %v, want errNotOwned", err)
	}
	if err := w.release(ctx, stale); !errors.Is(err, errNotOwned) {
		t.Fatalf("release from a stale claim: err = %v, want errNotOwned", err)
	}
	if got := get(t, q, job.ID); got.Status != StatusRunning || got.Attempts != 2 || got.LastError != nil {
		t.Fatalf("the live attempt's row was touched: %+v", got)
	}
	current := job
	current.Attempts = 2
	if err := w.complete(ctx, current); err != nil {
		t.Fatalf("complete from the live claim: %v", err)
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	// 4 bytes of ASCII, then a 3-byte rune that straddles the cut.
	s := "abcd" + strings.Repeat("é", 10)
	got := truncate(s, 5)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if got != "abcd…" {
		t.Fatalf("got %q, want %q", got, "abcd…")
	}
	if truncate("short", 10) != "short" {
		t.Fatal("short strings should be untouched")
	}
	if got := truncate("bad\xffbyte", 100); !utf8.ValidString(got) || !strings.Contains(got, "bad") {
		t.Fatalf("invalid input should be repaired: %q", got)
	}
}

// The same work enqueued again while a job runs leaves an identical job
// queued, and only one may be. The running one then cannot go back to the
// queue, on a failure or when its lock expires: it is finished with a note,
// the queued one does the work, and no other stale job is left behind.
func TestAJobWithAQueuedTwinIsFinishedNotRequeued(t *testing.T) {
	pool := dbtest.Pool(t)
	q := NewQueue(pool)
	ctx := context.Background()

	t.Run("on a failed attempt", func(t *testing.T) {
		var twin Job
		reg := Registry{"edited": func(ctx context.Context, job Job) error {
			var err error
			if twin, _, err = q.Enqueue(ctx, EnqueueInput{Kind: job.Kind, Payload: job.Payload, RunAt: time.Now().Add(time.Hour)}); err != nil {
				return err
			}
			return errors.New("the text changed; retrying")
		}}
		job, _, err := q.Enqueue(ctx, EnqueueInput{Kind: "edited", MaxAttempts: 3})
		if err != nil {
			t.Fatal(err)
		}
		stop := start(t, pool, reg, fast("w1", 1))
		waitFor(t, "the attempt to be recorded", func() bool { return status(t, q, job.ID) != string(StatusRunning) && get(t, q, job.ID).Attempts == 1 })
		stop()
		got := get(t, q, job.ID)
		if got.Status != StatusFailed || got.FinishedAt == nil || got.LastError == nil ||
			*got.LastError != "the text changed; retrying; "+twinNote {
			t.Fatalf("job with a queued twin: %+v (last_error %v)", got, got.LastError)
		}
		if twin.ID == job.ID || status(t, q, twin.ID) != string(StatusQueued) {
			t.Fatalf("the twin should be a second job, still queued: %+v", get(t, q, twin.ID))
		}
	})

	t.Run("when its lock expires", func(t *testing.T) {
		stale := func(kind string) (id int64) {
			if err := pool.QueryRow(ctx, `INSERT INTO jobs (kind, status, attempts, max_attempts, locked_by, locked_at)
				VALUES ($1, 'running', 1, 3, 'dead', now() - interval '1 hour') RETURNING id`, kind).Scan(&id); err != nil {
				t.Fatal(err)
			}
			return id
		}
		withTwin, first, second, other := stale("twinned"), stale("pair"), stale("pair"), stale("alone")
		twin, created, err := q.Enqueue(ctx, EnqueueInput{Kind: "twinned"})
		if err != nil || !created {
			t.Fatalf("enqueue the twin: created=%v err=%v", created, err)
		}

		w := NewWorker(pool, Registry{"x": func(context.Context, Job) error { return nil }}, WorkerConfig{ID: "sweeper", Logger: quiet})
		n, err := w.Reclaim(ctx, 5*time.Minute)
		if err != nil {
			t.Fatalf("reclaim: %v", err)
		}
		if n != 4 {
			t.Fatalf("reclaimed %d, want 4", n)
		}
		if got := get(t, q, withTwin); got.Status != StatusFailed || got.LastError == nil || !strings.HasSuffix(*got.LastError, twinNote) {
			t.Fatalf("stale job with a queued twin: %+v", got)
		}
		if status(t, q, twin.ID) != string(StatusQueued) {
			t.Fatalf("the twin: %s", status(t, q, twin.ID))
		}
		// Of two identical stale jobs, one goes back and the other does not.
		if a, b := status(t, q, first), status(t, q, second); a != string(StatusQueued) || b != string(StatusFailed) {
			t.Fatalf("two identical stale jobs: %s and %s, want queued and failed", a, b)
		}
		if got := status(t, q, other); got != string(StatusQueued) {
			t.Fatalf("an unrelated stale job was left %s", got)
		}
	})
}
