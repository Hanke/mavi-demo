package db

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a pool and retries briefly so the API survives the DB
// finishing its startup after compose reports it healthy.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		pool, err := pgxpool.New(ctx, url)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, lastErr
}

// Migrate applies every *.sql file in dir in lexical order, recording each in
// schema_migrations so it is skipped on the next run.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := sqlFiles(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		name := filepath.Base(f)
		var applied bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := execFile(ctx, pool, f); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			return err
		}
		log.Printf("applied %s", name)
	}
	return nil
}

// Seed runs every *.sql file in dir. Seed files are expected to be idempotent.
func Seed(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	files, err := sqlFiles(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := execFile(ctx, pool, f); err != nil {
			return fmt.Errorf("seed %s: %w", filepath.Base(f), err)
		}
		log.Printf("seeded %s", filepath.Base(f))
	}
	return nil
}

func sqlFiles(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func execFile(ctx context.Context, pool *pgxpool.Pool, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil
	}
	_, err = pool.Exec(ctx, string(b))
	return err
}
