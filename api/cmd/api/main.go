package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/db"
	"github.com/colehanke/mavi-demo/api/internal/server"
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

	// Subcommands used by the Makefile: `api migrate`, `api seed`.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			return db.Migrate(ctx, pool, envOr("MIGRATIONS_DIR", "/app/infra/db/migrations"))
		case "seed":
			return db.Seed(ctx, pool, envOr("SEED_DIR", "/app/infra/db/seed"))
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
