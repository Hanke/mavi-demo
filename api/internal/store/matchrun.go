package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Matching runs (the last stage: a reranked shortlist becomes the role's matches)
// ---------------------------------------------------------------------------

// RankedMatch is one candidate of a run's ranking, as it is written to matches.
type RankedMatch struct {
	CandidateID string
	Score       float64
	Explanation string
	Breakdown   json.RawMessage // an object with a "filter_run_id" key, which marks the row as a run's
}

// MatchRunResult is what ReplaceRunMatches did to the role's matches.
type MatchRunResult struct {
	Written       int // rows inserted or rewritten from this run
	PendingReview int // of those, how many are in the review queue
	Kept          int // ranked candidates whose match ops decided on, released or wrote by hand; left as they were
	Removed       int // undecided matches of earlier runs for candidates this run did not rank
}

// ErrSuperseded is ReplaceRunMatches refusing a run older than one whose
// matches are already written.
var ErrSuperseded = errors.New("superseded by a later matching run")

// replaceableMatch is true for a match a run may rewrite or remove: an
// earlier run wrote it (its breakdown names a filter run), nobody has decided
// on it and the employer cannot see it. `m` is the matches row.
const replaceableMatch = `(m.status IN ('proposed', 'pending_review') AND m.released_at IS NULL AND m.breakdown ? 'filter_run_id')`

// ReplaceRunMatches makes ranked, the reranked shortlist of filter run runID
// best first, the role's matches: one row per (role, candidate), so running a
// role again replaces what the earlier run wrote rather than adding to it.
// The first reviewSize of the ranking are written as pending_review, the
// review queue, and the rest as proposed. Nothing is released, so an
// employer sees none of it.
//
// What ops has done is not undone. A match that is approved, rejected,
// swapped or released, or that ops wrote by hand (POST /matches), keeps its
// score, explanation and status even when the run ranks its candidate again
// (counted in Kept); it still takes its place in the ranking, so it is not
// replaced in the queue by the next one down. A candidate deleted since the
// run is skipped and takes no place.
//
// A match an earlier run wrote, still undecided, for a candidate this run
// had no place for is deleted, or, when it has review history and cannot be
// (a release taken back), moved out of the queue to proposed. "No place"
// means the candidate failed the filters or fell below the retrieval limit.
// A candidate the run retrieved, or one who passed the filters but could not
// be compared yet (the run's unranked_ids), was not judged worse than
// anybody, so their match stays as it is whether or not they are in ranked.
//
// It is one transaction, with the role locked, so two runs of a role cannot
// interleave, and a run that started before one already written here is
// ErrSuperseded: a slow rerank does not overwrite a newer ranking. A run
// that no longer exists (the role was deleted) is ErrNotFound.
func (s *Store) ReplaceRunMatches(ctx context.Context, runID string, ranked []RankedMatch, reviewSize int) (MatchRunResult, error) {
	var res MatchRunResult
	type row struct {
		CandidateID string          `json:"candidate_id"`
		Rank        int             `json:"rank"`
		Score       float64         `json:"score"`
		Explanation string          `json:"explanation"`
		Breakdown   json.RawMessage `json:"breakdown"`
	}
	rows := make([]row, len(ranked))
	ids := make([]string, len(ranked))
	for i, m := range ranked {
		rows[i] = row{CandidateID: m.CandidateID, Rank: i, Score: m.Score, Explanation: m.Explanation, Breakdown: m.Breakdown}
		ids[i] = m.CandidateID
	}
	rowsJSON, err := json.Marshal(rows)
	if err != nil {
		return res, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op once committed

	var roleID string
	var ranAt time.Time
	var considered []string // who the run retrieved or could not compare
	if err := tx.QueryRow(ctx, `
		SELECT r.id::text, f.created_at, (f.retrieved_ids || f.unranked_ids)::text[]
		FROM filter_runs f JOIN roles r ON r.id = f.role_id
		WHERE f.id = $1 FOR NO KEY UPDATE OF r`, runID).Scan(&roleID, &ranAt, &considered); err != nil {
		return res, mapErr(err)
	}
	var superseded bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM filter_runs WHERE role_id = $1 AND matched_at IS NOT NULL AND created_at > $2)`,
		roleID, ranAt).Scan(&superseded); err != nil {
		return res, mapErr(err)
	}
	if superseded {
		return res, ErrSuperseded
	}

	// The queue is the first reviewSize of the ranking among the candidates
	// who still exist, counted here rather than by the caller.
	written, err := tx.Query(ctx, `
		INSERT INTO matches AS m (role_id, candidate_id, score, explanation, breakdown, status)
		SELECT $1::uuid, x.candidate_id, x.score, x.explanation, x.breakdown,
		       CASE WHEN row_number() OVER (ORDER BY x.rank) <= $3 THEN 'pending_review' ELSE 'proposed' END
		FROM jsonb_to_recordset($2::jsonb)
		     AS x (candidate_id uuid, rank int, score double precision, explanation text, breakdown jsonb)
		JOIN candidates c ON c.id = x.candidate_id
		ON CONFLICT (role_id, candidate_id) DO UPDATE SET
			score = EXCLUDED.score, explanation = EXCLUDED.explanation, breakdown = EXCLUDED.breakdown, status = EXCLUDED.status
		WHERE `+replaceableMatch+`
		RETURNING m.status`, roleID, string(rowsJSON), reviewSize)
	statuses, err := collect(written, err, func(r pgx.Row) (string, error) {
		var status string
		return status, r.Scan(&status)
	})
	if err != nil {
		return res, err
	}
	res.Written = len(statuses)
	for _, status := range statuses {
		if status == string(contract.MatchStatusPendingReview) {
			res.PendingReview++
		}
	}
	// Everything just written is replaceable, so what is not is what was kept.
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM matches m
		WHERE m.role_id = $1 AND m.candidate_id = ANY($2::text[]::uuid[]) AND NOT `+replaceableMatch,
		roleID, ids).Scan(&res.Kept); err != nil {
		return res, mapErr(err)
	}

	considered = append(considered, ids...)
	const stale = ` m.role_id = $1 AND m.candidate_id <> ALL($2::text[]::uuid[]) AND ` + replaceableMatch
	tag, err := tx.Exec(ctx, `
		DELETE FROM matches m
		WHERE`+stale+` AND NOT EXISTS (SELECT 1 FROM review_events e WHERE e.match_id = m.id)`, roleID, considered)
	if err != nil {
		return res, mapErr(err)
	}
	res.Removed = int(tag.RowsAffected())
	if _, err := tx.Exec(ctx, `UPDATE matches m SET status = 'proposed' WHERE`+stale+` AND m.status = 'pending_review'`, roleID, considered); err != nil {
		return res, mapErr(err)
	}

	if _, err := tx.Exec(ctx, `UPDATE filter_runs SET matched_at = now() WHERE id = $1`, runID); err != nil {
		return res, mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, mapErr(err)
	}
	return res, nil
}
