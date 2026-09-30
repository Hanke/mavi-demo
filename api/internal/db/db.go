package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
	"github.com/jackc/pgx/v5"
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

// Migration is one versioned schema change, discovered from a pair of files
// <version>.up.sql and <version>.down.sql in the migrations directory.
type Migration struct {
	Version  string // file stem, e.g. "0002_core_tables"; sorts lexically
	UpPath   string
	DownPath string // "" when no down file exists
}

// MigrationStatus is one row of Status: every known migration plus whether
// and when it was applied.
type MigrationStatus struct {
	Version      string
	Applied      bool
	AppliedAt    time.Time
	FilesMissing bool // applied, but no <Version>.up.sql exists in the directory
}

// migrationLock is an arbitrary constant; every migrate/rollback takes it so
// two runners cannot interleave.
const migrationLock = 0x6d617669 // "mavi"

const (
	upSuffix   = ".up.sql"
	downSuffix = ".down.sql"
)

// Migrate applies every pending migration in dir in version order. Each
// migration and its schema_migrations row commit in one transaction, so a
// failure leaves the database at the previous version.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	return withRunner(ctx, pool, func(r *runner) error {
		migrations, err := Discover(dir)
		if err != nil {
			return err
		}
		applied, err := r.applied(ctx)
		if err != nil {
			return err
		}
		n := 0
		for _, m := range migrations {
			if _, ok := applied[m.Version]; ok {
				continue
			}
			if err := r.apply(ctx, m); err != nil {
				return err
			}
			n++
		}
		if n == 0 {
			log.Printf("migrations up to date (%d applied)", len(applied))
		}
		return nil
	})
}

// MigrateDown rolls back the most recently applied migrations, newest first.
// steps <= 0 rolls back everything. A migration without a down file stops
// the rollback before anything is touched.
func MigrateDown(ctx context.Context, pool *pgxpool.Pool, dir string, steps int) error {
	return withRunner(ctx, pool, func(r *runner) error {
		migrations, err := Discover(dir)
		if err != nil {
			return err
		}
		byVersion := make(map[string]Migration, len(migrations))
		for _, m := range migrations {
			byVersion[m.Version] = m
		}
		applied, err := r.applied(ctx)
		if err != nil {
			return err
		}
		versions := make([]string, 0, len(applied))
		for v := range applied {
			versions = append(versions, v)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(versions)))
		if len(versions) == 0 {
			log.Printf("nothing to roll back")
			return nil
		}
		if steps > len(versions) {
			return fmt.Errorf("cannot roll back %d migrations: only %d applied (use \"all\" to roll back everything)", steps, len(versions))
		}
		if steps > 0 {
			versions = versions[:steps]
		}
		// Validate the whole batch up front so a missing down file in the
		// middle cannot leave a partial rollback.
		for _, v := range versions {
			m, ok := byVersion[v]
			if !ok {
				return fmt.Errorf("migration %s is applied but has no files in %s", v, dir)
			}
			if m.DownPath == "" {
				return fmt.Errorf("migration %s has no %s file", v, downSuffix)
			}
		}
		for _, v := range versions {
			if err := r.revert(ctx, byVersion[v]); err != nil {
				return err
			}
		}
		return nil
	})
}

// Status lists every migration on disk plus any applied version whose files
// are gone, in version order. It is read-only: no lock, no bookkeeping
// changes, and a database that has never been migrated reports everything
// as pending.
func Status(ctx context.Context, pool *pgxpool.Pool, dir string) ([]MigrationStatus, error) {
	migrations, err := Discover(dir)
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	r := &runner{conn: conn}
	applied, err := r.applied(ctx)
	if err != nil {
		return nil, err
	}
	var out []MigrationStatus
	seen := map[string]bool{}
	for _, m := range migrations {
		at, ok := applied[m.Version]
		out = append(out, MigrationStatus{Version: m.Version, Applied: ok, AppliedAt: at})
		seen[m.Version] = true
	}
	for v, at := range applied {
		if !seen[v] {
			out = append(out, MigrationStatus{Version: v, Applied: true, AppliedAt: at, FilesMissing: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Discover reads dir and pairs *.up.sql files with their *.down.sql
// counterparts. A down file with no up file is an error, since it can never
// be reached.
func Discover(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	byVersion := map[string]*Migration{}
	var downOnly []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, upSuffix):
			v := strings.TrimSuffix(name, upSuffix)
			m := byVersion[v]
			if m == nil {
				m = &Migration{Version: v}
				byVersion[v] = m
			}
			m.UpPath = filepath.Join(dir, name)
		case strings.HasSuffix(name, downSuffix):
			v := strings.TrimSuffix(name, downSuffix)
			m := byVersion[v]
			if m == nil {
				m = &Migration{Version: v}
				byVersion[v] = m
			}
			m.DownPath = filepath.Join(dir, name)
		case strings.HasSuffix(name, ".sql"):
			return nil, fmt.Errorf("migration %s must be named <version>%s or <version>%s", name, upSuffix, downSuffix)
		}
	}
	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.UpPath == "" {
			downOnly = append(downOnly, m.Version)
			continue
		}
		out = append(out, *m)
	}
	if len(downOnly) > 0 {
		sort.Strings(downOnly)
		return nil, fmt.Errorf("down migrations without an up file: %s", strings.Join(downOnly, ", "))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Querier is the subset of *pgxpool.Pool and pgx.Tx that read-only checks
// need, so a check can run inside the seed transaction or against a pool.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// SeedCheck inspects the seeded state and returns an error to reject it.
type SeedCheck func(ctx context.Context, q Querier) error

// Seed runs every *.sql file in dir, then every check, all inside one
// transaction: a file that fails or a check that rejects the data rolls the
// whole seed back, so a bad seed never lands. Seed files are expected to be
// idempotent.
func Seed(ctx context.Context, pool *pgxpool.Pool, dir string, checks ...SeedCheck) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, f := range files {
		sql, err := readSQL(f)
		if err != nil {
			return err
		}
		if sql == "" {
			continue
		}
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("seed %s: %w", filepath.Base(f), err)
		}
		log.Printf("seeded %s", filepath.Base(f))
	}
	for _, check := range checks {
		if err := check(ctx, tx); err != nil {
			return fmt.Errorf("seed rolled back: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// runner holds a single connection so the advisory lock and every migration
// transaction share a session.
type runner struct {
	conn *pgxpool.Conn
}

func withRunner(ctx context.Context, pool *pgxpool.Pool, fn func(*runner) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLock)
	}()
	r := &runner{conn: conn}
	if err := r.prepare(ctx); err != nil {
		return err
	}
	return fn(r)
}

// prepare creates the bookkeeping table and upgrades rows written by the
// original runner, which stored whole file names ("0001_documents.sql")
// rather than version stems.
func (r *runner) prepare(ctx context.Context) error {
	if _, err := r.conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	_, err := r.conn.Exec(ctx, `UPDATE schema_migrations
		SET name = left(name, -4)
		WHERE name LIKE '%.sql' AND name NOT LIKE '%`+upSuffix+`' AND name NOT LIKE '%`+downSuffix+`'`)
	return err
}

// applied returns the recorded versions. Names written by the original
// runner ("0001_documents.sql") are normalised on read so Status can stay
// read-only; prepare rewrites them in place before any up or down.
func (r *runner) applied(ctx context.Context) (map[string]time.Time, error) {
	var exists bool
	if err := r.conn.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	if !exists {
		return out, nil
	}
	rows, err := r.conn.Query(ctx, `SELECT name, applied_at FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var at time.Time
		if err := rows.Scan(&name, &at); err != nil {
			return nil, err
		}
		out[legacyVersion(name)] = at
	}
	return out, rows.Err()
}

// legacyVersion maps a whole-file name recorded by the original runner to its
// version stem. Stems and the new suffixed names pass through unchanged.
func legacyVersion(name string) string {
	if strings.HasSuffix(name, ".sql") && !strings.HasSuffix(name, upSuffix) && !strings.HasSuffix(name, downSuffix) {
		return strings.TrimSuffix(name, ".sql")
	}
	return name
}

func (r *runner) apply(ctx context.Context, m Migration) error {
	sql, err := readSQL(m.UpPath)
	if err != nil {
		return err
	}
	err = r.inTx(ctx, func(tx pgx.Tx) error {
		if sql != "" {
			if _, err := tx.Exec(ctx, sql); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, m.Version)
		return err
	})
	if err != nil {
		return fmt.Errorf("migration %s up: %w", m.Version, err)
	}
	log.Printf("applied %s", m.Version)
	return nil
}

func (r *runner) revert(ctx context.Context, m Migration) error {
	sql, err := readSQL(m.DownPath)
	if err != nil {
		return err
	}
	err = r.inTx(ctx, func(tx pgx.Tx) error {
		if sql != "" {
			if _, err := tx.Exec(ctx, sql); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE name = $1`, m.Version)
		return err
	})
	if err != nil {
		return fmt.Errorf("migration %s down: %w", m.Version, err)
	}
	log.Printf("rolled back %s", m.Version)
	return nil
}

func (r *runner) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.conn.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return errors.Join(err, rbErr)
		}
		return err
	}
	return tx.Commit(ctx)
}

func readSQL(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Offender is a hard-filter value that is not a canonical taxonomy id, found
// by NonCanonicalTerms. Suggestion is the id the value would resolve to, or
// "" when the taxonomy does not know it at all.
type Offender struct {
	Table      string
	Column     string
	Value      string
	Suggestion string
}

func (o Offender) String() string {
	s := fmt.Sprintf("%s.%s has %q", o.Table, o.Column, o.Value)
	if o.Suggestion != "" {
		return s + fmt.Sprintf(" (did you mean %q?)", o.Suggestion)
	}
	return s + " (not in the taxonomy; add it or keep it in the profile's free-text field)"
}

// hardFilterColumns are the array columns the shortlist query compares with
// `@>`. Their values must be canonical ids from the shared taxonomy or the
// containment check silently fails to match.
var hardFilterColumns = []struct {
	table, column string
	kind          taxonomy.Kind
}{
	{"candidate_profiles", "certifications", taxonomy.Certifications},
	{"candidate_profiles", "software", taxonomy.Software},
	{"roles", "required_certifications", taxonomy.Certifications},
	{"roles", "required_software", taxonomy.Software},
}

// NonCanonicalTerms scans every hard-filter array column and returns the
// distinct values that are not canonical taxonomy ids. `api seed` runs it as
// a SeedCheck inside the seed transaction, so a seed that drifts from
// infra/taxonomy.json is rolled back instead of producing a role that never
// matches anyone.
func NonCanonicalTerms(ctx context.Context, q Querier, tax *taxonomy.Taxonomy) ([]Offender, error) {
	var out []Offender
	for _, c := range hardFilterColumns {
		// Identifiers come from the fixed table above, never from input.
		rows, err := q.Query(ctx, fmt.Sprintf(
			`SELECT DISTINCT v FROM %s, unnest(%s) AS v ORDER BY v`, c.table, c.column))
		if err != nil {
			return nil, fmt.Errorf("scan %s.%s: %w", c.table, c.column, err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return nil, err
			}
			if tax.IsCanonical(c.kind, v) {
				continue
			}
			suggestion, _ := tax.Resolve(c.kind, v)
			out = append(out, Offender{Table: c.table, Column: c.column, Value: v, Suggestion: suggestion})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// TaxonomyCheck is the SeedCheck form of NonCanonicalTerms: it logs every
// offender and rejects the seed if there are any.
func TaxonomyCheck(tax *taxonomy.Taxonomy) SeedCheck {
	return func(ctx context.Context, q Querier) error {
		offenders, err := NonCanonicalTerms(ctx, q, tax)
		if err != nil {
			return fmt.Errorf("taxonomy check: %w", err)
		}
		if len(offenders) == 0 {
			log.Printf("taxonomy check: every hard-filter value is canonical")
			return nil
		}
		for _, o := range offenders {
			log.Printf("taxonomy check: %s", o)
		}
		return fmt.Errorf("%d hard-filter value(s) are not canonical taxonomy ids (see log above)", len(offenders))
	}
}
