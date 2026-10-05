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

// PromisedMatches is how many profiles the pipeline promises for a role. A
// run that cannot put that many forward needs attention (RunOutcome), and a
// role is ready once that many matches are released (RoleMatchStatus).
const PromisedMatches = 2

// MatchRunResult is what ReplaceRunMatches did to the role's matches.
type MatchRunResult struct {
	Outcome       RunOutcome
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

// RunOutcome is how a matching run ended, as recorded on its filter run:
// matched, or needs_attention with the reason. The zero value is no outcome
// yet (ReplaceRunMatches, of a run that is not whole).
type RunOutcome struct {
	Status contract.RunStatus
	Reason *contract.AttentionReason // nil unless Status is needs_attention
	Detail string                    // the AI service's error, for ai_failed
}

func needsAttention(reason contract.AttentionReason) RunOutcome {
	return RunOutcome{Status: contract.RunStatusNeedsAttention, Reason: &reason}
}

// FailRun records that the rerank of filter run runID failed (cause is the AI
// service's error): the run needs attention, as ai_failed. It touches no
// match, so the role's matches stay as the last run that finished left them,
// and a retry, which is a new run, has nothing of this one to duplicate. A
// run that is gone, or whose matches were written after all, is left alone.
func (s *Store) FailRun(ctx context.Context, runID string, cause error) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE filter_runs SET match_status = 'needs_attention', attention_reason = 'ai_failed', attention_detail = $2
		WHERE id = $1 AND matched_at IS NULL`, runID, cause.Error())
	return mapErr(err)
}

// ReplaceRunMatches makes ranked, the reranked shortlist of filter run runID
// best first, the role's matches: one row per (role, candidate), so running a
// role again replaces what the earlier run wrote rather than adding to it.
// The first reviewSize of the ranking are written as pending_review, the
// review queue, unless they score below minScore; those, and the rest, are
// written as proposed. Nothing is released, so an employer sees none of it.
//
// A weak candidate is never put in the queue to make up the number. When
// fewer than PromisedMatches candidates passed the run's hard filters, or
// fewer than that many of ranked qualify, the run is recorded as
// needs_attention (MatchRunResult.Outcome) and the queue is however many did
// qualify, which may be nobody; otherwise it is recorded as matched. To
// qualify is to score minScore or more and still be someone who can be put
// forward: a candidate deleted since the run, or whose match ops rejected or
// swapped out, is not.
//
// partial says the run ranked only some of those who passed, with the rest
// still being embedded and a later run on its way for them. Too few
// qualifying is then not a finding yet, and the run is left with no outcome
// rather than recorded as needing attention; too few passing is one already.
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
func (s *Store) ReplaceRunMatches(ctx context.Context, runID string, ranked []RankedMatch, reviewSize int, minScore float64, partial bool) (MatchRunResult, error) {
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
	var passed int
	if err := tx.QueryRow(ctx, `
		SELECT r.id::text, f.created_at, (f.retrieved_ids || f.unranked_ids)::text[], f.after_timezone_overlap
		FROM filter_runs f JOIN roles r ON r.id = f.role_id
		WHERE f.id = $1 FOR NO KEY UPDATE OF r`, runID).Scan(&roleID, &ranAt, &considered, &passed); err != nil {
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
	// who still exist, counted here rather than by the caller, less whoever
	// scores below minScore.
	written, err := tx.Query(ctx, `
		INSERT INTO matches AS m (role_id, candidate_id, score, explanation, breakdown, status)
		SELECT $1::uuid, x.candidate_id, x.score, x.explanation, x.breakdown,
		       CASE WHEN row_number() OVER (ORDER BY x.rank) <= $3 AND x.score >= $4 THEN 'pending_review' ELSE 'proposed' END
		FROM jsonb_to_recordset($2::jsonb)
		     AS x (candidate_id uuid, rank int, score double precision, explanation text, breakdown jsonb)
		JOIN candidates c ON c.id = x.candidate_id
		ON CONFLICT (role_id, candidate_id) DO UPDATE SET
			score = EXCLUDED.score, explanation = EXCLUDED.explanation, breakdown = EXCLUDED.breakdown, status = EXCLUDED.status
		WHERE `+replaceableMatch+`
		RETURNING m.status`, roleID, string(rowsJSON), reviewSize, minScore)
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

	// Who of the ranking can be put forward, by this run's scores and the
	// matches as they now stand.
	var qualified int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM jsonb_to_recordset($2::jsonb) AS x (candidate_id uuid, score double precision)
		JOIN matches m ON m.role_id = $1 AND m.candidate_id = x.candidate_id
		WHERE x.score >= $3 AND m.status NOT IN ('rejected', 'swapped')`,
		roleID, string(rowsJSON), minScore).Scan(&qualified); err != nil {
		return res, mapErr(err)
	}
	switch {
	case passed < PromisedMatches:
		res.Outcome = needsAttention(contract.AttentionReasonTooFewPassed)
	case qualified >= PromisedMatches:
		res.Outcome = RunOutcome{Status: contract.RunStatusMatched}
	case !partial:
		res.Outcome = needsAttention(contract.AttentionReasonTooFewQualified)
	}

	// Nothing was scored when nobody was ranked, so there is no threshold to record.
	var status, scored, threshold any
	if res.Outcome.Status != "" {
		status = res.Outcome.Status
	}
	if len(ranked) > 0 {
		scored, threshold = qualified, minScore
	}
	if _, err := tx.Exec(ctx, `
		UPDATE filter_runs SET matched_at = now(), match_status = $2, attention_reason = $3, attention_detail = NULL,
			min_score = $4, qualified = $5
		WHERE id = $1`, runID, status, res.Outcome.Reason, threshold, scored); err != nil {
		return res, mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, mapErr(err)
	}
	return res, nil
}

// RoleMatchStatus is where a role's matching stands, for ops: ready once
// PromisedMatches matches are released; needs_attention when it is not and
// the latest matching run with an outcome could not deliver them; in_review
// otherwise, which includes a role no run has finished for. Run is that
// latest run, nil when there is none. A role that does not exist is
// ErrNotFound.
//
// What an employer may be told of this is the caller's to cut down.
func (s *Store) RoleMatchStatus(ctx context.Context, roleID string) (contract.RoleMatchStatus, error) {
	out := contract.RoleMatchStatus{Status: contract.RoleMatchStateInReview}
	if err := s.pool.QueryRow(ctx, `
		SELECT r.id::text, (SELECT count(*) FROM matches m WHERE m.role_id = r.id AND m.released_at IS NOT NULL)
		FROM roles r WHERE r.id = $1`, roleID).Scan(&out.RoleID, &out.Released); err != nil {
		return out, mapErr(err)
	}
	run, err := scanFilterRun(s.pool.QueryRow(ctx, `
		SELECT `+filterRunCols+` FROM filter_runs
		WHERE role_id = $1 AND match_status IS NOT NULL
		ORDER BY created_at DESC, id LIMIT 1`, roleID))
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return out, err
	default:
		out.Run = &run
	}
	switch {
	case out.Released >= PromisedMatches:
		out.Status = contract.RoleMatchStateReady
	case out.Run != nil && *out.Run.MatchStatus == contract.RunStatusNeedsAttention:
		out.Status = contract.RoleMatchStateNeedsAttention
	}
	return out, nil
}
