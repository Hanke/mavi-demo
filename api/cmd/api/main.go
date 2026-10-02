package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // the alpine image has no zoneinfo; handlers validate IANA names

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/db"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/colehanke/mavi-demo/api/internal/server"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/tasks"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, envOr("DATABASE_URL", "postgres://mavi:mavi@localhost:5432/mavi?sslmode=disable"))
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	// Subcommands used by the Makefile:
	//   api migrate [up]          apply pending migrations
	//   api migrate down [N|all]  roll back the last N (default 1) or all
	//   api migrate status        list migrations and whether they are applied
	//   api seed                  load seed data, then check the hard-filter
	//                             columns against the shared taxonomy
	//   api worker                run only the background job worker (no HTTP);
	//                             for extra workers next to the API process
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			return runMigrate(ctx, pool, os.Args[2:])
		case "seed":
			return runSeed(ctx, pool)
		case "worker":
		default:
			return fmt.Errorf("unknown command %q", os.Args[1])
		}
	}

	tax, err := taxonomy.Load(taxonomyPath())
	if err != nil {
		return err
	}
	ai := aiclient.New(envOr("AI_SERVICE_URL", "http://localhost:8000"))
	// MATCH_RETRIEVAL_SIZE is how many of the candidates who pass a role's
	// hard filters go on to the rerank, which takes at most 50.
	retrieve := envInt("MATCH_RETRIEVAL_SIZE", store.DefaultRetrievalLimit)
	if retrieve < 1 || retrieve > store.MaxRetrievalLimit {
		return fmt.Errorf("MATCH_RETRIEVAL_SIZE: want 1 to %d, got %d", store.MaxRetrievalLimit, retrieve)
	}
	// MATCH_REVIEW_SIZE is how many of a matching run's ranking, from the top,
	// go to ops as pending_review.
	review := envInt("MATCH_REVIEW_SIZE", tasks.DefaultReviewSize)
	if review < 1 || review > store.MaxRetrievalLimit {
		return fmt.Errorf("MATCH_REVIEW_SIZE: want 1 to %d, got %d", store.MaxRetrievalLimit, review)
	}
	handlers := tasks.Registry(pool, ai, tax, tasks.MatchConfig{Retrieve: retrieve, ReviewSize: review})
	if len(os.Args) > 1 { // worker
		return newWorker(pool, handlers, envInt("WORKER_CONCURRENCY", 2)).Run(ctx)
	}
	srv := &http.Server{
		Addr: ":" + envOr("PORT", "8080"),
		Handler: server.New(server.Config{
			DB:       pool,
			AI:       ai,
			Store:    store.New(pool),
			Taxonomy: tax,
			Jobs:     jobs.NewQueue(pool),
			JobKinds: handlers.Kinds(),
			EmbedRole: func(ctx context.Context, roleID string) error {
				return tasks.EmbedRole(ctx, pool, ai, roleID)
			},
			RetrievalSize: retrieve,
			CORSOrigin:    envOr("CORS_ORIGIN", "http://localhost:5173"),
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// The API process runs a worker of its own so `make up` needs no extra
	// service. WORKER_CONCURRENCY=0 turns it off (run `api worker` elsewhere).
	workerDone := make(chan struct{})
	if n := envInt("WORKER_CONCURRENCY", 2); n > 0 {
		w := newWorker(pool, handlers, n)
		go func() {
			defer close(workerDone)
			if err := w.Run(ctx); err != nil {
				log.Printf("worker: %v", err)
			}
		}()
	} else {
		close(workerDone)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("api listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	<-workerDone // in-flight jobs finish or are handed back before the pool closes
	return nil
}

// newWorker builds the job worker from the environment:
//
//	WORKER_ID            name written to jobs.locked_by; default hostname-pid
//	WORKER_CONCURRENCY   jobs run at once; default 2, 0 disables the embedded worker
//	WORKER_LOCK_TIMEOUT  how long a 'running' job may go unfinished before another
//	                     worker reclaims it; default 5m, must exceed the slowest job
func newWorker(pool *pgxpool.Pool, handlers jobs.Registry, concurrency int) *jobs.Worker {
	cfg := jobs.WorkerConfig{ID: os.Getenv("WORKER_ID"), Concurrency: concurrency}
	if v := os.Getenv("WORKER_LOCK_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("WORKER_LOCK_TIMEOUT: %v", err)
		}
		cfg.LockTimeout = d
	}
	w := jobs.NewWorker(pool, handlers, cfg)
	log.Printf("worker %s: concurrency %d, kinds %v", w.ID(), concurrency, handlers.Kinds())
	return w
}

// runSeed loads the seed files and rolls them back if any hard-filter column
// would hold a value that is not a canonical id in infra/taxonomy.json. The
// shortlist query compares those columns with `@>`, so a stray "QBO" in a seed
// would silently never match a profile that says "quickbooks".
func runSeed(ctx context.Context, pool *pgxpool.Pool) error {
	tax, err := taxonomy.Load(taxonomyPath())
	if err != nil {
		return err
	}
	return db.Seed(ctx, pool, envOr("SEED_DIR", "/app/infra/db/seed"), db.TaxonomyCheck(tax))
}

func runMigrate(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	dir := envOr("MIGRATIONS_DIR", "/app/infra/db/migrations")
	sub := "up"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "up":
		return db.Migrate(ctx, pool, dir)
	case "down":
		steps := 1
		if len(args) > 1 {
			if args[1] == "all" {
				steps = 0
			} else {
				n, err := strconv.Atoi(args[1])
				if err != nil || n < 1 {
					return fmt.Errorf("migrate down: expected a positive step count or \"all\", got %q", args[1])
				}
				steps = n
			}
		}
		return db.MigrateDown(ctx, pool, dir, steps)
	case "status":
		statuses, err := db.Status(ctx, pool, dir)
		if err != nil {
			return err
		}
		for _, s := range statuses {
			state := "pending"
			if s.Applied {
				state = "applied " + s.AppliedAt.UTC().Format(time.RFC3339)
			}
			if s.FilesMissing {
				state += " (files missing)"
			}
			fmt.Printf("%-40s %s\n", s.Version, state)
		}
		return nil
	default:
		return fmt.Errorf("unknown migrate command %q (want up, down or status)", sub)
	}
}

// taxonomyPath honours TAXONOMY_PATH, then the container mount, then the
// monorepo layout for `go run ./cmd/api` from api/.
func taxonomyPath() string {
	if p := os.Getenv("TAXONOMY_PATH"); p != "" {
		return p
	}
	for _, p := range []string{"/app/infra/taxonomy.json", "../infra/taxonomy.json"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/app/infra/taxonomy.json"
}

// envInt reads an integer variable; a value that is not an integer is fatal
// rather than silently taking the default.
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("%s: want an integer, got %q", key, v)
	}
	return n
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
