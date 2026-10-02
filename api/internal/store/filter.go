package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Hard filters (the first stage of a matching run; one filter_runs row each)
// ---------------------------------------------------------------------------

type FilterRun = contract.FilterRun

// filterOrder is the order the hard filters are applied in, which is the
// order of the after_* columns of filter_runs and of FilterRun.Stages.
var filterOrder = []contract.FilterName{
	contract.FilterNameProfile,
	contract.FilterNameCertifications,
	contract.FilterNameSoftware,
	contract.FilterNameExperience,
	contract.FilterNameAvailability,
	contract.FilterNameTimezoneOverlap,
}

const filterRunCols = `id::text, role_id::text, overlap_on, pool, after_profile, after_certifications, after_software,
	after_experience, after_availability, after_timezone_overlap, candidate_ids::text[], created_at`

func scanFilterRun(row pgx.Row) (FilterRun, error) {
	var run FilterRun
	var on time.Time
	after := make([]int, len(filterOrder))
	dest := []any{&run.ID, &run.RoleID, &on, &run.Pool}
	for i := range after {
		dest = append(dest, &after[i])
	}
	if err := row.Scan(append(dest, &run.CandidateIds, &run.CreatedAt)...); err != nil {
		return run, mapErr(err)
	}
	run.OverlapOn = contract.Date(on)
	run.CandidateIds = nonNil(run.CandidateIds)
	run.Stages = make([]contract.FilterStage, len(filterOrder))
	before := run.Pool
	for i, name := range filterOrder {
		run.Stages[i] = contract.FilterStage{Filter: name, Remaining: after[i], Excluded: before - after[i]}
		before = after[i]
	}
	run.Passed = before
	return run, nil
}

// RunHardFilter narrows the active candidates to those who meet every
// must-have of the role, in one statement, and records the run: who passed
// (FilterRun.CandidateIds, by name) and how many were left after each filter.
// A later stage of a matching run takes its candidates from the result and
// from nowhere else, so a candidate missing a must-have never reaches it.
//
// acceptable is asked, for the role as stored, for one entry per required
// qualification: the certification ids that satisfy it (taxonomy.Acceptable).
// A candidate needs one from each, and an empty entry is a requirement nobody
// can meet. The role row is locked against edits from that read until the run
// is recorded, so the sets and the columns the statement reads are the same
// version of the role. today is the day the time-zone overlap is worked out
// for when the role has no start date. A role that does not exist is
// ErrNotFound.
//
// Each filter is a boolean per candidate (the `checked` CTE) rather than a
// WHERE clause, so the same pass that picks the candidates counts where the
// others fell out. A missing profile, figure or answer is false, never NULL:
// what is not known does not pass. The overlap is worked out once per
// distinct set of working hours, not once per candidate (`hours`), and only
// between zones Postgres lists by that exact name (`zones`): AT TIME ZONE
// also takes abbreviations, POSIX strings such as "XYZ5" and names in the
// wrong case, none of which the API accepts as a zone.
func (s *Store) RunHardFilter(ctx context.Context, roleID string, today time.Time, acceptable func(Role) [][]string) (FilterRun, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FilterRun{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op once committed

	role, err := scanRole(tx.QueryRow(ctx, `SELECT `+roleCols+` FROM roles WHERE id = $1 FOR SHARE`, roleID))
	if err != nil {
		return FilterRun{}, err
	}
	sets := acceptable(role)
	if sets == nil {
		sets = [][]string{}
	}
	setsJSON, err := json.Marshal(sets)
	if err != nil {
		return FilterRun{}, err
	}
	y, m, d := today.Date()
	run, err := scanFilterRun(tx.QueryRow(ctx, `
		WITH role AS (
			SELECT *, coalesce(starts_on, $2::date) AS on_day FROM roles WHERE id = $1
		),
		zones AS MATERIALIZED (
			SELECT name FROM pg_timezone_names
		),
		hours AS MATERIALIZED (
			SELECT h.timezone, h.work_start, h.work_end,
			       overlap_minutes(r.timezone, h.timezone, h.work_start, h.work_end, r.on_day) AS minutes
			FROM role r
			CROSS JOIN (
				SELECT DISTINCT a.timezone, a.work_start, a.work_end
				FROM candidate_availability a
				JOIN candidates c ON c.id = a.candidate_id AND c.status = 'active'
			) h
			WHERE r.timezone IN (SELECT name FROM zones) AND h.timezone IN (SELECT name FROM zones)
		),
		checked AS (
			SELECT c.id, c.full_name,
			       p.candidate_id IS NOT NULL AS has_profile,
			       NOT EXISTS (
			           SELECT 1 FROM jsonb_array_elements($3::jsonb) AS q (ids)
			           WHERE NOT coalesce(p.certifications && ARRAY(SELECT jsonb_array_elements_text(q.ids)), false)
			       ) AS certifications,
			       coalesce(p.software @> r.required_software, false) AS software,
			       -- The API stores "no minimum" as NULL; a 0 written past it means the same.
			       coalesce(r.min_years_experience, 0) = 0
			           OR coalesce(p.years_experience >= r.min_years_experience, false) AS experience,
			       a.candidate_id IS NOT NULL
			           AND (r.starts_on IS NULL OR a.available_from <= r.starts_on)
			           AND (r.hours_per_week IS NULL OR a.hours_per_week >= r.hours_per_week) AS availability,
			       -- As availability.Check: with a zone, the overlap must be computable
			       -- even when no minimum is set; without one, a minimum cannot be met.
			       CASE WHEN r.timezone IS NULL THEN r.min_overlap_hours IS NULL
			            ELSE coalesce(h.minutes >= coalesce(r.min_overlap_hours, 0) * 60, false)
			       END AS timezone_overlap
			FROM role r
			JOIN candidates c ON c.status = 'active'
			LEFT JOIN candidate_profiles p ON p.candidate_id = c.id
			LEFT JOIN candidate_availability a ON a.candidate_id = c.id
			LEFT JOIN hours h ON h.timezone = a.timezone AND h.work_start = a.work_start AND h.work_end = a.work_end
		),
		staged AS (
			-- How many filters the candidate got through, in filterOrder.
			SELECT id, full_name,
			       CASE WHEN NOT has_profile      THEN 0
			            WHEN NOT certifications   THEN 1
			            WHEN NOT software         THEN 2
			            WHEN NOT experience       THEN 3
			            WHEN NOT availability     THEN 4
			            WHEN NOT timezone_overlap THEN 5
			            ELSE 6
			       END AS cleared
			FROM checked
		)
		INSERT INTO filter_runs (role_id, overlap_on, pool, after_profile, after_certifications, after_software,
			after_experience, after_availability, after_timezone_overlap, candidate_ids)
		SELECT r.id, r.on_day, count(k.id),
		       count(*) FILTER (WHERE k.cleared >= 1), count(*) FILTER (WHERE k.cleared >= 2),
		       count(*) FILTER (WHERE k.cleared >= 3), count(*) FILTER (WHERE k.cleared >= 4),
		       count(*) FILTER (WHERE k.cleared >= 5), count(*) FILTER (WHERE k.cleared >= 6),
		       coalesce(array_agg(k.id ORDER BY k.full_name, k.id) FILTER (WHERE k.cleared >= 6), '{}')
		FROM role r
		LEFT JOIN staged k ON true
		GROUP BY r.id, r.on_day
		RETURNING `+filterRunCols,
		role.ID, time.Date(y, m, d, 0, 0, 0, 0, time.UTC), string(setsJSON)))
	if err != nil {
		return FilterRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FilterRun{}, mapErr(err)
	}
	return run, nil
}

// ListFilterRuns is the recorded runs for a role, newest first.
func (s *Store) ListFilterRuns(ctx context.Context, roleID string, p Page) ([]FilterRun, error) {
	p = p.clamp()
	rows, err := s.pool.Query(ctx, `
		SELECT `+filterRunCols+` FROM filter_runs
		WHERE role_id = $1
		ORDER BY created_at DESC, id
		LIMIT $2 OFFSET $3`, roleID, p.Limit, p.Offset)
	return collect(rows, err, scanFilterRun)
}
