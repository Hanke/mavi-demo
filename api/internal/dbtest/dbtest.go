// Package dbtest gives integration tests a throwaway, fully migrated
// database. Tests skip unless TEST_DATABASE_URL points at a Postgres the
// test user can CREATE DATABASE on (the compose DB works:
// postgres://mavi:mavi@localhost:5433/mavi?sslmode=disable).
package dbtest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool creates a fresh database, applies infra/db/migrations, and drops the
// database when the test ends.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("mavi_test_%d_%d", time.Now().UnixNano(), rand.IntN(1<<20))
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	if err := db.Migrate(ctx, pool, MigrationsDir()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// RepoRoot is the monorepo root, located relative to this source file.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func MigrationsDir() string { return filepath.Join(RepoRoot(), "infra", "db", "migrations") }
func TaxonomyPath() string  { return filepath.Join(RepoRoot(), "infra", "taxonomy.json") }
