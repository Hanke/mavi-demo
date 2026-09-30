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

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/db"
	"github.com/colehanke/mavi-demo/api/internal/server"
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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			return runMigrate(ctx, pool, os.Args[2:])
		case "seed":
			return runSeed(ctx, pool)
		default:
			return fmt.Errorf("unknown command %q", os.Args[1])
		}
	}

	ai := aiclient.New(envOr("AI_SERVICE_URL", "http://localhost:8000"))
	srv := &http.Server{
		Addr:              ":" + envOr("PORT", "8080"),
		Handler:           server.New(pool, ai, envOr("CORS_ORIGIN", "http://localhost:5173")),
		ReadHeaderTimeout: 5 * time.Second,
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
	return nil
}

// runSeed loads the seed files and rolls them back if any hard-filter column
// would hold a value that is not a canonical id in infra/taxonomy.json. The
// shortlist query compares those columns with `@>`, so a stray "QBO" in a seed
// would silently never match a profile that says "quickbooks".
func runSeed(ctx context.Context, pool *pgxpool.Pool) error {
	tax, err := taxonomy.Load(envOr("TAXONOMY_PATH", "/app/infra/taxonomy.json"))
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
