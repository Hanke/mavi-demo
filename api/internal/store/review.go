package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Human review (ops decides on a role's matches and releases two of them)
// ---------------------------------------------------------------------------
//
// Every method here that changes a match writes its review_events row in the
// same transaction, so the audit trail cannot drift from what it records, and
// these are the only writers of a decision: neither CreateMatch nor
// UpdateMatch can approve, reject, swap or release.
//
// Each locks the role first, as ReplaceRunMatches does, so a decision and a
// matching run of the same role, or two decisions, do not interleave.

// Reviewer is who is making a decision, for the audit trail.
type Reviewer struct {
	Actor  string // the ops user, as the request identified them
	Reason string // optional; "" is recorded as NULL
}

// RefusedError is a review action the role's matches do not allow as they
// stand: a release without exactly two approved, a swap with nobody to bring
// in. Nothing was written.
type RefusedError struct {
	Reason string
}

func (e *RefusedError) Error() string { return e.Reason }

func refused(format string, args ...any) error {
	return &RefusedError{Reason: fmt.Sprintf(format, args...)}
}

// matchRanking orders a role's matches best first: by score, then, between
// equal scores, by the place the matching run gave them.
const matchRanking = ` m.score DESC,
	CASE WHEN jsonb_typeof(m.breakdown -> 'rank') = 'number' THEN (m.breakdown ->> 'rank')::numeric END NULLS LAST, m.id `

// nextRanked selects the reserve of role $1, best first: the matches a swap
// may bring into the queue. They are undecided and outside the queue, their
// candidate is still in the pool, and they score $2 or more, the same bar a
// matching run sets for the queue.
const nextRanked = matchFrom + `
	WHERE m.role_id = $1 AND m.status = 'proposed' AND m.released_at IS NULL AND m.score >= $2 AND c.status = 'active'
	ORDER BY` + matchRanking

// ReviewQueue is what ops has to decide on for a role and what it has
// decided: the pending_review matches, the approved ones, and the next
// ranked candidate, whom SwapMatch would bring in (minScore as there). A role
// that does not exist is ErrNotFound.
func (s *Store) ReviewQueue(ctx context.Context, roleID string, minScore float64) (contract.ReviewQueue, error) {
	out := contract.ReviewQueue{Pending: []Match{}, Approved: []Match{}}
	if err := s.pool.QueryRow(ctx, `SELECT id::text FROM roles WHERE id = $1`, roleID).Scan(&out.RoleID); err != nil {
		return out, mapErr(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+matchCols+matchFrom+`
		WHERE m.role_id = $1 AND m.status IN ('pending_review', 'approved')
		ORDER BY`+matchRanking, out.RoleID)
	matches, err := collect(rows, err, scanMatch)
	if err != nil {
		return out, err
	}
	for _, m := range matches {
		if m.Status == contract.MatchStatusApproved {
			out.Approved = append(out.Approved, m)
		} else {
			out.Pending = append(out.Pending, m)
		}
	}
	next, err := scanMatch(s.pool.QueryRow(ctx, `SELECT `+matchCols+nextRanked+` LIMIT 1`, out.RoleID, minScore))
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return out, err
	default:
		out.Next = &next
	}
	return out, nil
}

// reviewed is a match as a decision finds it, locked with its role.
type reviewed struct {
	roleID   string
	status   contract.MatchStatus
	released bool
}

// lockForReview locks the role of match id and then the match, and reads the
// match as it stands once both are held. ErrNotFound when there is no such
// match.
func lockForReview(ctx context.Context, tx pgx.Tx, id string) (reviewed, error) {
	var m reviewed
	if err := tx.QueryRow(ctx, `
		SELECT r.id::text FROM matches m JOIN roles r ON r.id = m.role_id
		WHERE m.id = $1 FOR NO KEY UPDATE OF r`, id).Scan(&m.roleID); err != nil {
		return m, mapErr(err)
	}
	err := tx.QueryRow(ctx, `SELECT status, released_at IS NOT NULL FROM matches WHERE id = $1 FOR UPDATE`, id).Scan(&m.status, &m.released)
	return m, mapErr(err)
}

// decide is ApproveMatch and RejectMatch: it moves match id to status `to`
// and records the action, unless the match is there already, which is a
// no-op with no audit row (a double-click, a retried request). allowed says
// whether the match may be decided on as it stands.
func (s *Store) decide(ctx context.Context, id string, to contract.MatchStatus, action contract.ReviewAction, by Reviewer, allowed func(reviewed) error) (Match, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Match{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op once committed

	m, err := lockForReview(ctx, tx, id)
	if err != nil {
		return Match{}, err
	}
	if m.status != to {
		if err := allowed(m); err != nil {
			return Match{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE matches SET status = $2 WHERE id = $1`, id, to); err != nil {
			return Match{}, mapErr(err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO review_events (match_id, action, actor, reason, metadata)
			VALUES ($1, $2, $3, NULLIF($4, ''), jsonb_build_object('from', $5::text))`,
			id, action, by.Actor, by.Reason, m.status); err != nil {
			return Match{}, mapErr(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Match{}, mapErr(err)
		}
	}
	return s.GetMatch(ctx, id, false)
}

// ApproveMatch approves an undecided match (in the queue, or proposed) and
// records who did. Approving shows the employer nothing: that is ReleaseRole.
// A match that was rejected or swapped out is a RefusedError; one already
// approved is left as it is.
func (s *Store) ApproveMatch(ctx context.Context, id string, by Reviewer) (Match, error) {
	return s.decide(ctx, id, contract.MatchStatusApproved, contract.ReviewActionApprove, by, func(m reviewed) error {
		if m.status != contract.MatchStatusProposed && m.status != contract.MatchStatusPendingReview {
			return refused("the match is %s: a candidate who was turned down cannot be approved", m.status)
		}
		return nil
	})
}

// RejectMatch turns a candidate down with nobody brought in, and records who
// did: an undecided match, or an approved one, which is how an approval is
// taken back. A released match must be withdrawn first and one that was
// swapped out stays that; both are a RefusedError. One already rejected is
// left as it is.
func (s *Store) RejectMatch(ctx context.Context, id string, by Reviewer) (Match, error) {
	return s.decide(ctx, id, contract.MatchStatusRejected, contract.ReviewActionReject, by, func(m reviewed) error {
		switch {
		case m.status == contract.MatchStatusSwapped:
			return refused("the match is already swapped out")
		case m.released:
			return refused("the match is released to the employer: withdraw it before rejecting it")
		}
		return nil
	})
}

// SwapMatch takes match id out of review and brings in the next ranked
// candidate of its role: the match becomes swapped and the best of the
// reserve (nextRanked, scoring minScore or more) pending_review, where it
// waits for a decision like any other. One swap event records both.
//
// A RefusedError, with nothing changed, when the match is not in review
// (pending_review or approved), when it is released, or when nobody is in
// reserve: a weak candidate is not brought in to make up the number.
func (s *Store) SwapMatch(ctx context.Context, id string, minScore float64, by Reviewer) (out contract.SwapResult, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op once committed

	m, err := lockForReview(ctx, tx, id)
	if err != nil {
		return out, err
	}
	switch {
	case m.status == contract.MatchStatusSwapped:
		return out, refused("the match is already swapped out")
	case m.status != contract.MatchStatusPendingReview && m.status != contract.MatchStatusApproved:
		return out, refused("the match is %s, not in review: only a candidate in the queue or approved can be swapped out", m.status)
	case m.released:
		return out, refused("the match is released to the employer: withdraw it before swapping it out")
	}
	var nextID, nextCandidate string
	err = tx.QueryRow(ctx, `SELECT m.id::text, m.candidate_id::text`+nextRanked+` LIMIT 1 FOR UPDATE OF m`, m.roleID, minScore).Scan(&nextID, &nextCandidate)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, refused("there is no next ranked candidate: nobody in reserve for this role scores %g or more", minScore)
	}
	if err != nil {
		return out, mapErr(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE matches SET status = CASE WHEN id = $1 THEN 'swapped' ELSE 'pending_review' END
		WHERE id IN ($1, $2)`, id, nextID); err != nil {
		return out, mapErr(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO review_events (match_id, action, actor, reason, replacement_candidate_id, metadata)
		VALUES ($1, 'swap', $2, NULLIF($3, ''), $4, jsonb_build_object('from', $5::text, 'replacement_match_id', $6::text))`,
		id, by.Actor, by.Reason, nextCandidate, m.status, nextID); err != nil {
		return out, mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return out, mapErr(err)
	}
	if out.Swapped, err = s.GetMatch(ctx, id, false); err != nil {
		return out, err
	}
	out.Replacement, err = s.GetMatch(ctx, nextID, false)
	return out, err
}

// ReleaseRole releases a role's approved matches to the employer, and is the
// only thing that releases a match. It is a RefusedError, with nothing
// changed, unless exactly PromisedMatches of the role's matches are approved:
// with fewer there are not two profiles to show, and with more ops has not
// said which two. It is also refused while a match that is not approved is
// released (one released before approval was required), since the employer
// would see that one too.
//
// Each match it releases gets a release event. One already released is left
// as it is, so releasing a role again is a no-op with no audit rows. It
// returns the role's released matches, best first: what the employer sees. A
// role that does not exist is ErrNotFound.
func (s *Store) ReleaseRole(ctx context.Context, roleID string, by Reviewer) ([]Match, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op once committed

	if err := tx.QueryRow(ctx, `SELECT id::text FROM roles WHERE id = $1 FOR NO KEY UPDATE`, roleID).Scan(&roleID); err != nil {
		return nil, mapErr(err)
	}
	var approved, unapproved int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'approved'),
		       count(*) FILTER (WHERE status <> 'approved' AND released_at IS NOT NULL)
		FROM matches WHERE role_id = $1`, roleID).Scan(&approved, &unapproved); err != nil {
		return nil, mapErr(err)
	}
	switch {
	case approved != PromisedMatches:
		return nil, refused("a release needs exactly %d approved candidates, and this role has %d", PromisedMatches, approved)
	case unapproved > 0:
		return nil, refused("%d released match(es) of this role are not approved: withdraw them before releasing", unapproved)
	}
	if _, err := tx.Exec(ctx, `
		WITH released AS (
			UPDATE matches SET released_at = now()
			WHERE role_id = $1 AND status = 'approved' AND released_at IS NULL
			RETURNING id
		)
		INSERT INTO review_events (match_id, action, actor, reason)
		SELECT id, 'release', $2, NULLIF($3, '') FROM released`, roleID, by.Actor, by.Reason); err != nil {
		return nil, mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapErr(err)
	}
	return s.ListMatches(ctx, MatchFilter{RoleID: roleID, ReleasedOnly: true}, Page{})
}

// UnreleaseMatch withdraws a released match from the employer and records
// who did. The match keeps its status, so an approved one is released again
// by ReleaseRole. One that is not released is left as it is, with no audit
// row.
func (s *Store) UnreleaseMatch(ctx context.Context, id string, by Reviewer) (Match, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Match{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op once committed

	m, err := lockForReview(ctx, tx, id)
	if err != nil {
		return Match{}, err
	}
	if m.released {
		if _, err := tx.Exec(ctx, `UPDATE matches SET released_at = NULL WHERE id = $1`, id); err != nil {
			return Match{}, mapErr(err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO review_events (match_id, action, actor, reason) VALUES ($1, 'unrelease', $2, NULLIF($3, ''))`,
			id, by.Actor, by.Reason); err != nil {
			return Match{}, mapErr(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Match{}, mapErr(err)
		}
	}
	return s.GetMatch(ctx, id, false)
}

// ListReviewEvents is the audit trail of a role: every decision recorded on
// one of its matches, oldest first.
func (s *Store) ListReviewEvents(ctx context.Context, roleID string, p Page) ([]contract.ReviewEvent, error) {
	p = p.clamp()
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.match_id::text, m.role_id::text, m.candidate_id::text, c.full_name, e.action, e.actor, e.reason,
		       e.replacement_candidate_id::text, rc.full_name, e.metadata, e.created_at
		FROM review_events e
		JOIN matches m ON m.id = e.match_id
		JOIN candidates c ON c.id = m.candidate_id
		LEFT JOIN candidates rc ON rc.id = e.replacement_candidate_id
		WHERE m.role_id = $1
		ORDER BY e.id
		LIMIT $2 OFFSET $3`, roleID, p.Limit, p.Offset)
	return collect(rows, err, func(row pgx.Row) (contract.ReviewEvent, error) {
		var e contract.ReviewEvent
		err := row.Scan(&e.ID, &e.MatchID, &e.RoleID, &e.CandidateID, &e.CandidateName, &e.Action, &e.Actor, &e.Reason,
			&e.ReplacementCandidateID, &e.ReplacementCandidateName, &e.Metadata, &e.CreatedAt)
		return e, mapErr(err)
	})
}
