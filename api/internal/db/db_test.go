package db

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDiscoverPairsUpAndDown(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "0002_b.up.sql", "select 2")
	write(t, dir, "0002_b.down.sql", "select -2")
	write(t, dir, "0001_a.up.sql", "select 1")
	write(t, dir, "notes.txt", "ignored")

	got, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Version != "0001_a" || got[1].Version != "0002_b" {
		t.Fatalf("unexpected migrations: %+v", got)
	}
	if got[0].DownPath != "" {
		t.Errorf("0001_a should have no down file, got %q", got[0].DownPath)
	}
	if filepath.Base(got[1].DownPath) != "0002_b.down.sql" {
		t.Errorf("0002_b down = %q", got[1].DownPath)
	}
}

func TestDiscoverRejectsUnpairedAndLegacyNames(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "0001_a.down.sql", "")
	if _, err := Discover(dir); err == nil || !strings.Contains(err.Error(), "0001_a") {
		t.Fatalf("expected error about orphan down file, got %v", err)
	}

	dir = t.TempDir()
	write(t, dir, "0001_a.sql", "")
	if _, err := Discover(dir); err == nil || !strings.Contains(err.Error(), "must be named") {
		t.Fatalf("expected error about legacy name, got %v", err)
	}
}

// TestMigrateRoundTrip runs the real infra/db/migrations up, down and up again
// against a throwaway database. Set TEST_DATABASE_URL to a superuser
// connection (the compose DB works: postgres://mavi:mavi@localhost:5433/mavi).
func TestMigrateRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	dir := filepath.Join("..", "..", "..", "infra", "db", "migrations")

	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	var allVersions []string
	for _, m := range migrations {
		allVersions = append(allVersions, m.Version)
	}
	wantApplied := strings.Join(allVersions, ",")

	// A never-migrated database reports everything pending without creating
	// schema_migrations as a side effect.
	statuses, err := Status(ctx, pool, dir)
	if err != nil {
		t.Fatalf("status on fresh db: %v", err)
	}
	if len(statuses) != len(migrations) {
		t.Fatalf("status on fresh db: got %d rows, want %d", len(statuses), len(migrations))
	}
	for _, s := range statuses {
		if s.Applied || s.FilesMissing {
			t.Errorf("status on fresh db: %s should be pending, got %+v", s.Version, s)
		}
	}
	if tableExists(t, pool, "schema_migrations") {
		t.Fatal("Status must not create schema_migrations")
	}

	// Simulate a database migrated by the original runner, which recorded the
	// whole file name. The new runner must recognise it as 0001_documents.
	exec(t, pool, `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`)
	exec(t, pool, `INSERT INTO schema_migrations (name) VALUES ('0001_documents.sql')`)
	exec(t, pool, `CREATE TABLE documents (id int)`)

	statuses, err = Status(ctx, pool, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statuses {
		if s.Version == "0001_documents" && (!s.Applied || s.FilesMissing) {
			t.Errorf("status should normalise the legacy row on read, got %+v", s)
		}
	}
	if got := appliedVersions(t, pool); strings.Join(got, ",") != "0001_documents.sql" {
		t.Fatalf("Status must not rewrite rows, got %v", got)
	}

	if err := Migrate(ctx, pool, dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	wantTables := []string{"documents", "candidates", "candidate_profiles", "roles", "matches", "review_events", "jobs"}
	for _, tbl := range wantTables {
		if !tableExists(t, pool, tbl) {
			t.Errorf("after up: table %s missing", tbl)
		}
	}
	if !queryBool(t, pool, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`) {
		t.Error("after up: vector extension not installed")
	}
	if !queryBool(t, pool, `SELECT EXISTS (
		SELECT 1 FROM pg_indexes WHERE tablename = 'candidate_profiles'
		AND indexname = 'candidate_profiles_embedding_idx' AND indexdef ILIKE '%USING hnsw%')`) {
		t.Error("after up: hnsw index on candidate_profiles.embedding missing")
	}
	for _, col := range []string{"certifications", "software", "availability", "timezone"} {
		if !queryBool(t, pool, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_name = 'candidate_profiles' AND column_name = $1)`, col) {
			t.Errorf("after up: hard-filter column candidate_profiles.%s missing", col)
		}
	}
	if got := appliedVersions(t, pool); strings.Join(got, ",") != wantApplied {
		t.Fatalf("after up: applied = %v, want %s", got, wantApplied)
	}

	// Exercise the hard-filter columns with a real query.
	exec(t, pool, `INSERT INTO candidates (id, full_name) VALUES ('00000000-0000-0000-0000-000000000001', 'A')`)
	exec(t, pool, `INSERT INTO candidate_profiles (candidate_id, certifications, software, availability, timezone)
		VALUES ('00000000-0000-0000-0000-000000000001', '{pmp,cpa}', '{salesforce}', 'immediate', 'America/Chicago')`)
	if !queryBool(t, pool, `SELECT EXISTS (SELECT 1 FROM candidate_profiles
		WHERE certifications @> '{pmp}' AND software @> '{salesforce}'
		AND availability IN ('immediate', 'two_weeks') AND timezone = 'America/Chicago')`) {
		t.Error("hard-filter query did not find the seeded profile")
	}

	// Second up is a no-op.
	if err := Migrate(ctx, pool, dir); err != nil {
		t.Fatalf("second up: %v", err)
	}

	// More steps than applied is refused, not rounded down to "all".
	if err := MigrateDown(ctx, pool, dir, len(allVersions)+1); err == nil || !strings.Contains(err.Error(), "only") {
		t.Fatalf("expected oversized step count to be refused, got %v", err)
	}
	if !tableExists(t, pool, "jobs") {
		t.Fatal("refused rollback must not touch the schema")
	}

	// Down one step reverts only the newest migration.
	if err := MigrateDown(ctx, pool, dir, 1); err != nil {
		t.Fatalf("down 1: %v", err)
	}
	if got := appliedVersions(t, pool); strings.Join(got, ",") != strings.Join(allVersions[:len(allVersions)-1], ",") {
		t.Errorf("after down 1: applied = %v, want all but the last", got)
	}
	if !tableExists(t, pool, "candidates") {
		t.Error("after down 1: candidates should still exist")
	}
	// Rolling back through the jobs migration drops that table and nothing else.
	jobsIdx := slices.IndexFunc(allVersions, func(v string) bool { return strings.HasSuffix(v, "_jobs") })
	if jobsIdx < 0 {
		t.Fatal("no *_jobs migration found")
	}
	if err := MigrateDown(ctx, pool, dir, len(allVersions)-1-jobsIdx); err != nil {
		t.Fatalf("down through jobs: %v", err)
	}
	if tableExists(t, pool, "jobs") {
		t.Error("after rolling back 0003_jobs: jobs still exists")
	}
	if !tableExists(t, pool, "candidates") {
		t.Error("after rolling back 0003_jobs: candidates should still exist")
	}

	// Down everything leaves only the bookkeeping table.
	if err := MigrateDown(ctx, pool, dir, 0); err != nil {
		t.Fatalf("down all: %v", err)
	}
	for _, tbl := range wantTables {
		if tableExists(t, pool, tbl) {
			t.Errorf("after down all: table %s still exists", tbl)
		}
	}
	if got := appliedVersions(t, pool); len(got) != 0 {
		t.Fatalf("after down all: applied = %v", got)
	}
	if queryBool(t, pool, `SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'set_updated_at')`) {
		t.Error("after down all: set_updated_at() still exists")
	}
	if queryBool(t, pool, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname IN ('vector', 'pgcrypto'))`) {
		t.Error("after down all: extensions still installed")
	}

	// And back up again from a truly empty database: no extensions, no tables.
	if err := Migrate(ctx, pool, dir); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got := appliedVersions(t, pool); strings.Join(got, ",") != wantApplied {
		t.Fatalf("after re-up: applied = %v, want %s", got, wantApplied)
	}

	statuses, err = Status(ctx, pool, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statuses {
		if !s.Applied || s.FilesMissing {
			t.Errorf("status: %s reported %+v", s.Version, s)
		}
	}
}

func TestStatusReportsAppliedVersionWithoutFiles(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	dir := t.TempDir()
	write(t, dir, "0001_ok.up.sql", "SELECT 1")
	write(t, dir, "0001_ok.down.sql", "SELECT 1")
	if err := Migrate(ctx, pool, dir); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO schema_migrations (name) VALUES ('0002_gone')`)

	statuses, err := Status(ctx, pool, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[1].Version != "0002_gone" || !statuses[1].FilesMissing {
		t.Fatalf("unexpected statuses: %+v", statuses)
	}
	if statuses[0].FilesMissing {
		t.Errorf("0001_ok should not be flagged: %+v", statuses[0])
	}
}

func TestMigrateFailureRollsBackWholeMigration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	dir := t.TempDir()
	write(t, dir, "0001_ok.up.sql", "CREATE TABLE ok (id int)")
	write(t, dir, "0001_ok.down.sql", "DROP TABLE ok")
	write(t, dir, "0002_bad.up.sql", "CREATE TABLE partial (id int); SELECT 1/0")
	write(t, dir, "0002_bad.down.sql", "DROP TABLE partial")

	err := Migrate(ctx, pool, dir)
	if err == nil || !strings.Contains(err.Error(), "0002_bad") {
		t.Fatalf("expected 0002_bad to fail, got %v", err)
	}
	if !tableExists(t, pool, "ok") {
		t.Error("0001_ok should have been applied and committed")
	}
	if tableExists(t, pool, "partial") {
		t.Error("0002_bad's partial work should have been rolled back")
	}
	if got := appliedVersions(t, pool); strings.Join(got, ",") != "0001_ok" {
		t.Errorf("applied = %v", got)
	}

	// A migration without a down file blocks the rollback before anything runs.
	if err := os.Remove(filepath.Join(dir, "0001_ok.down.sql")); err != nil {
		t.Fatal(err)
	}
	if err := MigrateDown(ctx, pool, dir, 0); err == nil || !strings.Contains(err.Error(), ".down.sql") {
		t.Fatalf("expected missing-down error, got %v", err)
	}
	if !tableExists(t, pool, "ok") {
		t.Error("ok should be untouched when the rollback is refused")
	}
}

// testPool creates a throwaway database on the server TEST_DATABASE_URL points
// at and drops it when the test ends.
func testPool(t *testing.T) *pgxpool.Pool {
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
	return pool
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func queryBool(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) bool {
	t.Helper()
	var b bool
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&b); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return b
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	return queryBool(t, pool, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`, name)
}

func appliedVersions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// TestSeedValuesAreCanonical loads the real seed files into a migrated
// throwaway database and checks every hard-filter column against the shared
// taxonomy, which is exactly what `api seed` does after seeding.
func TestSeedValuesAreCanonical(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	infra := filepath.Join("..", "..", "..", "infra")
	if err := Migrate(ctx, pool, filepath.Join(infra, "db", "migrations")); err != nil {
		t.Fatal(err)
	}
	if err := Seed(ctx, pool, filepath.Join(infra, "db", "seed")); err != nil {
		t.Fatal(err)
	}
	tax, err := taxonomy.Load(filepath.Join(infra, "taxonomy.json"))
	if err != nil {
		t.Fatal(err)
	}
	offenders, err := NonCanonicalTerms(ctx, pool, tax)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offenders {
		t.Errorf("seed data: %s", o)
	}

	// A role written with an alias instead of the id is caught, with the
	// canonical id suggested; a value the taxonomy does not know is reported
	// without a suggestion.
	exec(t, pool, `INSERT INTO roles (title, required_certifications, required_software)
		VALUES ('bad', '{"Certified Public Accountant"}', '{QBO,"Zoho Books"}')`)
	offenders, err = NonCanonicalTerms(ctx, pool, tax)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, o := range offenders {
		got[o.Table+"."+o.Column+":"+o.Value] = o.Suggestion
	}
	want := map[string]string{
		"roles.required_certifications:Certified Public Accountant": "cpa",
		"roles.required_software:QBO":                               "quickbooks",
		"roles.required_software:Zoho Books":                        "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("offenders = %v, want %v", got, want)
	}
}

// TestSeedRollsBackWhenCheckFails: a seed whose data fails a check must leave
// no rows behind, and the real taxonomy check is such a check.
func TestSeedRollsBackWhenCheckFails(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	infra := filepath.Join("..", "..", "..", "infra")
	if err := Migrate(ctx, pool, filepath.Join(infra, "db", "migrations")); err != nil {
		t.Fatal(err)
	}
	tax, err := taxonomy.Load(filepath.Join(infra, "taxonomy.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write(t, dir, "001_bad.sql", `INSERT INTO roles (title, required_software) VALUES ('bad', '{QBO}')`)

	err = Seed(ctx, pool, dir, TaxonomyCheck(tax))
	if err == nil || !strings.Contains(err.Error(), "rolled back") || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("expected the taxonomy check to reject the seed, got %v", err)
	}
	if queryBool(t, pool, `SELECT EXISTS (SELECT 1 FROM roles)`) {
		t.Error("rejected seed left rows in roles")
	}

	// The same file with the canonical id passes the check and commits.
	write(t, dir, "001_bad.sql", `INSERT INTO roles (title, required_software) VALUES ('good', '{quickbooks}')`)
	if err := Seed(ctx, pool, dir, TaxonomyCheck(tax)); err != nil {
		t.Fatalf("canonical seed should pass: %v", err)
	}
	if !queryBool(t, pool, `SELECT EXISTS (SELECT 1 FROM roles WHERE title = 'good')`) {
		t.Error("accepted seed did not commit")
	}
}
