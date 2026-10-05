package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultReviewSize is how many of a run's ranking go to the review queue
// when MatchConfig does not say.
const DefaultReviewSize = 5

// DefaultMinScore is the least a candidate may score and still be put forward
// when MatchConfig does not say. A score is the rubric's levels weighted
// (docs/rerank-rubric.md): 0.5 is "partial" on every dimension, 0.75
// "strong", so this asks for better than partial overall.
const DefaultMinScore = 0.6

// The AI service's limits on what /rerank reads, in characters:
// MAX_TEXT_CHARS (ai/app/main.py) for the role and MAX_CANDIDATE_CHARS
// (ai/app/extract.py) for each candidate. Longer text is cut to fit rather
// than refused, so one long resume does not fail the whole run.
const (
	maxRerankRoleChars      = 60_000
	maxRerankCandidateChars = 20_000
)

// A shortlist is reranked in batches, one /rerank call each, all at once: a
// call holds at most rerankBatchSize candidates and rerankBatchChars
// characters of their text (one candidate is never split), so no prompt grows
// with MATCH_RETRIEVAL_SIZE and the run takes as long as its slowest call,
// which keeps it inside the worker's lock timeout.
const (
	rerankBatchSize  = 10
	rerankBatchChars = 100_000
)

// How far ahead a run queues the one that follows it: matchRetryDelay when it
// could match nobody and is waiting for embeddings, matchPickupDelay when it
// matched who it could and some candidates are still being embedded. The
// second is a whole rerank again, so it is given time to find them all done.
const (
	matchRetryDelay  = 30 * time.Second
	matchPickupDelay = 5 * time.Minute
)

// MatchConfig sizes a matching run.
type MatchConfig struct {
	// Retrieve is how many of the candidates who pass the hard filters are
	// reranked; below 1 is store.DefaultRetrievalLimit.
	Retrieve int
	// ReviewSize is how many of the ranking, from the top, are written as
	// pending_review; below 1 is DefaultReviewSize.
	ReviewSize int
	// MinScore is the minimum quality threshold: a candidate scoring below it
	// is never put in the review queue, and a run with fewer than
	// store.PromisedMatches at or above it needs attention. 0 or less is
	// DefaultMinScore.
	MinScore float64
}

// Reranker is the one AI call a matching run makes.
type Reranker interface {
	Rerank(ctx context.Context, role string, candidates []aiclient.RerankCandidate) (aiclient.RerankResponse, error)
}

type matcher struct {
	pool  *pgxpool.Pool
	ai    Reranker
	tax   *taxonomy.Taxonomy
	store *store.Store
	queue *jobs.Queue
	cfg   MatchConfig
}

// run is a matching run for a role, end to end: the hard filters and the
// retrieval by embedding (HardFilter, one filter_runs row), the rerank of
// that shortlist by the AI service, and the result written to matches
// (Store.ReplaceRunMatches) with the top of the ranking marked
// pending_review for ops. Nothing is released, so the employer sees nothing
// until ops says so.
//
// The candidates reranked are the run's Retrieved and no others. Each match
// carries the rerank's score, an explanation written from its reasons and
// its evidence for each dimension of the rubric, and a breakdown with the
// dimensions as returned (level, evidence and the quotes from the
// candidate's text), so the score can be recomputed and every claim traced
// to the resume.
//
// It is safe to run again, for a retry or because the role or the pool
// changed: the matches are keyed on (role, candidate) and replaced, never
// added to. A role that is not open (filled or closed since the job was
// queued) is not matched.
//
// The pipeline promises two profiles (store.PromisedMatches), and a run that
// cannot deliver them still completes, recorded on its filter run as
// needs_attention with the reason, where ops finds it with the funnel and the
// must-have that eliminated the most candidates (FilterRun.TopFilter; GET
// /roles/{id}/match-status):
//
//   - too_few_passed: zero or one candidate passed the hard filters. The one,
//     if any, is still reranked and written.
//   - too_few_qualified: fewer than two of the ranking score MinScore or
//     more, not counting anyone ops has rejected or swapped out. Everyone is
//     written, for ops to read, but nobody below the threshold enters the
//     review queue: a weak candidate is not promoted to make up the number.
//     A run that could not rank everybody yet (some are still being
//     embedded, and a later run is queued for them) records no outcome
//     instead: it is not known yet.
//   - ai_failed: the AI service failed or timed out for any batch of the
//     rerank. Nothing of the run is written to matches, the error is
//     returned and the queue retries the job while it has attempts; the retry
//     is a new run, so there is nothing of this one to duplicate.
//
// A run that delivers them is recorded as matched.
//
// What cannot be judged yet is never written as an empty result. A role with
// no embedding, or a pool in which nobody who passed has one, waits: while a
// job that will produce the missing embedding is queued or running, this job
// ends and queues a later run in its place (later), for as long as that
// takes; with no such job it fails the attempt, and the queue's own retries
// cover an embedding that landed in between. When only some of those who
// passed are waiting, the rest are matched now, the waiting keep whatever
// match they had, and a later run is queued to pick them up.
func (m *matcher) run(ctx context.Context, job jobs.Job) error {
	var p struct {
		RoleID string `json:"role_id"`
	}
	if err := json.Unmarshal(job.Payload, &p); err != nil || strings.TrimSpace(p.RoleID) == "" {
		return jobs.PayloadError(job, `{"role_id": uuid}`)
	}
	var status string
	var embedded bool
	err := m.pool.QueryRow(ctx, `SELECT status, coalesce(vector_norm(embedding) > 0, false) FROM roles WHERE id = $1`, p.RoleID).Scan(&status, &embedded)
	if done, err := rowGone(job, err); done {
		return err
	}
	if status != string(contract.RoleStatusOpen) {
		logf("match: job %d: role %s is %s, not open; not matched", job.ID, p.RoleID, status)
		return nil
	}
	notEmbedded := fmt.Errorf("job %d: role %s is not embedded yet and no %s job is waiting to embed it; retrying", job.ID, p.RoleID, KindEmbedRole)
	if !embedded {
		// Checked before the filters so that waiting does not record a run each time.
		return m.wait(ctx, job, notEmbedded, "role_id", []string{p.RoleID}, KindEmbedRole)
	}

	run, err := HardFilter(ctx, m.store, m.tax, p.RoleID, time.Now(), m.cfg.Retrieve)
	if errors.Is(err, store.ErrNotFound) {
		return nil // deleted since the read above; nothing to match
	}
	if err != nil {
		return err
	}
	if !run.RoleEmbedded {
		return notEmbedded // the embedding was cleared by an edit between the two reads
	}
	if len(run.Retrieved) == 0 && len(run.UnrankedIds) > 0 {
		return m.wait(ctx, job, fmt.Errorf("job %d: %d candidates pass the filters for role %s but none has an embedding to compare with the role's, and no job is waiting to embed one; retrying",
			job.ID, len(run.UnrankedIds), p.RoleID), "candidate_id", run.UnrankedIds, KindEmbedProfile, KindParseResume)
	}

	ranked, err := m.rerank(ctx, job, run)
	if err != nil {
		return err
	}
	review := m.cfg.ReviewSize
	if review < 1 {
		review = DefaultReviewSize
	}
	minScore := m.cfg.MinScore
	if minScore <= 0 {
		minScore = DefaultMinScore
	}
	// Whoever passed but is still being embedded gets their turn in a later
	// run, and until then this one has not seen everybody.
	partial := false
	if len(run.UnrankedIds) > 0 {
		if partial, err = m.embedding(ctx, "candidate_id", run.UnrankedIds, KindEmbedProfile, KindParseResume); err != nil {
			return err
		}
	}
	res, err := m.store.ReplaceRunMatches(ctx, run.ID, ranked, review, minScore, partial)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil // the role was deleted while its candidates were reranked
	case errors.Is(err, store.ErrSuperseded):
		logf("match: job %d: role %s: run %s not written: a later run already was", job.ID, p.RoleID, run.ID)
		return nil
	case err != nil:
		return err
	}
	logf("match: role %s: run %s: %d reranked, %d written (%d pending review), %d already decided and kept, %d from earlier runs removed",
		p.RoleID, run.ID, len(ranked), res.Written, res.PendingReview, res.Kept, res.Removed)
	if reason := res.Outcome.Reason; reason != nil {
		top := "no must-have excluded anybody"
		if f := run.TopFilter; f != nil {
			top = fmt.Sprintf("%s excluded the most candidates (%d)", f.Filter, f.Excluded)
		}
		logf("match: role %s: run %s needs attention: %s (%d passed the filters; minimum score %g); %s",
			p.RoleID, run.ID, *reason, run.Passed, minScore, top)
	}

	if partial {
		return m.later(ctx, job, matchPickupDelay, fmt.Sprintf("%d candidates who pass the filters are still being embedded", len(run.UnrankedIds)))
	}
	return nil
}

// wait is what a run does when it cannot go on without an embedding: if a job
// of one of kinds is queued or running for one of ids (the payload's field),
// the run ends here and one to take its place is queued for later; if not,
// notReady is returned, which costs the job an attempt.
func (m *matcher) wait(ctx context.Context, job jobs.Job, notReady error, field string, ids []string, kinds ...string) error {
	pending, err := m.embedding(ctx, field, ids, kinds...)
	if err != nil {
		return err
	}
	if !pending {
		return notReady
	}
	return m.later(ctx, job, matchRetryDelay, "waiting for "+strings.Join(kinds, " / "))
}

// embedding reports whether a job of one of kinds is queued or running for
// one of ids: an embedding that is still on its way.
func (m *matcher) embedding(ctx context.Context, field string, ids []string, kinds ...string) (pending bool, err error) {
	err = m.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM jobs WHERE kind = ANY($1) AND status IN ('queued', 'running') AND payload->>$2 = ANY($3))`,
		kinds, field, ids).Scan(&pending)
	return pending, err
}

// later queues the same job again, delay from now. The one running ends as
// succeeded: it did what could be done. One waiting already is reused, not
// added to.
func (m *matcher) later(ctx context.Context, job jobs.Job, delay time.Duration, why string) error {
	next, _, err := m.queue.Enqueue(ctx, jobs.EnqueueInput{
		Kind: KindMatchRole, Payload: job.Payload, Priority: job.Priority, MaxAttempts: job.MaxAttempts,
		RunAt: time.Now().Add(delay),
	})
	if err != nil {
		return fmt.Errorf("job %d: queue the next %s: %w", job.ID, KindMatchRole, err)
	}
	logf("match: job %d: %s; job %d runs it again", job.ID, why, next.ID)
	return nil
}

// rerank sends the run's shortlist to the AI service, in batches (batches),
// and returns it as matches, best first: the batches' results merged by
// score, equal scores in the order the service gave within a batch and in
// order of retrieval between batches. A shortlist of nobody is a ranking of
// nobody, with no call made; one whose candidates are all gone or have
// nothing to read since the filters ran a moment ago is not, and is retried
// against the pool as it is now.
func (m *matcher) rerank(ctx context.Context, job jobs.Job, run store.FilterRun) ([]store.RankedMatch, error) {
	if len(run.Retrieved) == 0 {
		return nil, nil
	}
	similarity := make(map[string]float64, len(run.Retrieved))
	ids := make([]string, len(run.Retrieved))
	for i, r := range run.Retrieved {
		ids[i], similarity[r.CandidateID] = r.CandidateID, r.Similarity
	}
	candidates, err := m.candidateTexts(ctx, ids)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("job %d: none of the %d candidates retrieved for role %s can be read any more (deleted, or no resume or profile text); retrying",
			job.ID, len(ids), run.RoleID)
	}
	role, err := m.roleText(ctx, run.RoleID)
	if err != nil {
		return nil, err
	}

	groups := batches(candidates)
	responses := make([]aiclient.RerankResponse, len(groups))
	errs := make([]error, len(groups))
	var wg sync.WaitGroup
	for i, group := range groups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses[i], errs[i] = m.ai.Rerank(ctx, role, group)
		}()
	}
	wg.Wait()
	// A 4xx means this request can never be scored. The rest, including the
	// 502 the service answers when the model's output does not validate, is
	// worth another attempt. Nothing is kept of a run with a failed batch,
	// which is recorded as needing attention either way.
	if err := errors.Join(errs...); err != nil {
		m.aiFailed(ctx, job, run, err)
		if errors.Is(err, aiclient.ErrBadRequest) {
			return nil, jobs.Permanent(err)
		}
		return nil, err
	}

	first := responses[0]
	var results []aiclient.RerankResult
	for _, resp := range responses {
		if resp.RubricVersion != first.RubricVersion {
			err := fmt.Errorf("job %d: the batches were scored under rubric versions %s and %s, so their scores do not compare; retrying",
				job.ID, first.RubricVersion, resp.RubricVersion)
			m.aiFailed(ctx, job, run, err)
			return nil, err
		}
		results = append(results, resp.Results...)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })

	ranked := make([]store.RankedMatch, len(results))
	for i, r := range results {
		breakdown, err := json.Marshal(matchBreakdown{
			FilterRunID:   run.ID,
			Rank:          i + 1,
			Similarity:    similarity[r.ID],
			RubricVersion: first.RubricVersion,
			Provider:      first.Provider,
			Dimensions:    r.Dimensions,
			Reasons:       append([]string{}, r.Reasons...),
		})
		if err != nil {
			return nil, jobs.Permanent(fmt.Errorf("job %d: breakdown for candidate %s: %w", job.ID, r.ID, err))
		}
		ranked[i] = store.RankedMatch{
			CandidateID: r.ID,
			// The service rounds to three decimals; as a float32 that is 0.736000001…
			Score:       math.Round(float64(r.Score)*1000) / 1000,
			Explanation: explanation(r),
			Breakdown:   breakdown,
		}
	}
	return ranked, nil
}

// aiFailed records on the run that its rerank failed, and why
// (Store.FailRun). The job is failing with cause already, so an error here is
// only logged. A job interrupted by a shutdown did not fail: it is handed
// back to the queue and nothing is recorded.
func (m *matcher) aiFailed(ctx context.Context, job jobs.Job, run store.FilterRun, cause error) {
	if ctx.Err() != nil {
		return
	}
	if err := m.store.FailRun(ctx, run.ID, cause); err != nil {
		logf("match: job %d: run %s: could not record the failed rerank: %v", job.ID, run.ID, err)
		return
	}
	logf("match: role %s: run %s needs attention: ai_failed: %v", run.RoleID, run.ID, cause)
}

// batches splits a shortlist, in order, into the groups sent to /rerank: each
// is at most rerankBatchSize candidates and rerankBatchChars characters of
// text, and never empty.
func batches(candidates []aiclient.RerankCandidate) [][]aiclient.RerankCandidate {
	var out [][]aiclient.RerankCandidate
	chars := 0
	for _, c := range candidates {
		n := utf8.RuneCountInString(c.Text)
		if last := len(out) - 1; last < 0 || len(out[last]) >= rerankBatchSize || chars+n > rerankBatchChars {
			out = append(out, nil)
			chars = 0
		}
		out[len(out)-1] = append(out[len(out)-1], c)
		chars += n
	}
	return out
}

// matchBreakdown is matches.breakdown as a run writes it: where the match
// came from (the filter run, the candidate's place in its ranking and their
// embedding similarity to the role) and the rerank result behind the score,
// with the rubric version it follows. filter_run_id is also what marks a
// match as a run's, for the next run to replace (Store.ReplaceRunMatches).
type matchBreakdown struct {
	FilterRunID   string                   `json:"filter_run_id"`
	Rank          int                      `json:"rank"`
	Similarity    float64                  `json:"similarity"`
	RubricVersion string                   `json:"rubric_version"`
	Provider      string                   `json:"provider"`
	Dimensions    aiclient.DimensionScores `json:"dimensions"`
	Reasons       []string                 `json:"reasons"`
}

// levelNames is what each rubric level is called (LEVEL_NAMES in ai/app/rubric.py).
var levelNames = [...]string{"none", "weak", "partial", "strong", "full"}

// explanation is the match's rationale for ops, in words: the rerank's
// reasons, then one line per dimension of the rubric, in the rubric's order
// (docs/rerank-rubric.md), with the level and the sentence of evidence for
// it. The quotes that back each line are in the breakdown.
func explanation(r aiclient.RerankResult) string {
	d := r.Dimensions
	depth := int(d.ExperienceDepth.Level)
	lines := []struct {
		label    string
		level    *int
		evidence string
	}{
		{"Must-have coverage", optionalLevel(d.MustHaveCoverage), d.MustHaveCoverage.Evidence},
		{"Relevant experience depth", &depth, d.ExperienceDepth.Evidence},
		{"Software fluency", optionalLevel(d.SoftwareFluency), d.SoftwareFluency.Evidence},
		{"Industry fit", optionalLevel(d.IndustryFit), d.IndustryFit.Evidence},
		{"Nice-to-haves", optionalLevel(d.NiceToHaves), d.NiceToHaves.Evidence},
	}
	var b strings.Builder
	for _, reason := range r.Reasons {
		if reason = strings.TrimSpace(reason); reason != "" {
			b.WriteString(reason)
			b.WriteByte('\n')
		}
	}
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	for _, l := range lines {
		b.WriteString(l.label)
		switch {
		case l.level == nil:
			b.WriteString(": not scored for this role.")
		case *l.level >= 0 && *l.level < len(levelNames):
			fmt.Fprintf(&b, ": %s (%d/%d).", levelNames[*l.level], *l.level, len(levelNames)-1)
		default:
			fmt.Fprintf(&b, ": level %d.", *l.level)
		}
		if evidence := strings.TrimSpace(l.evidence); evidence != "" {
			b.WriteByte(' ')
			b.WriteString(evidence)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func optionalLevel(d aiclient.OptionalDimensionScore) *int {
	if d.Level == nil {
		return nil
	}
	level := int(*d.Level)
	return &level
}

// candidateTexts is what the rerank judges each candidate on, in the order
// of ids: their resume as uploaded, or, for a candidate with a profile but no
// resume text (one written through the API), the profile as profileText
// renders it. A candidate deleted since the run, or with nothing to read, is
// left out.
func (m *matcher) candidateTexts(ctx context.Context, ids []string) ([]aiclient.RerankCandidate, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT c.id::text, c.resume_text, p.headline, p.profile, p.certifications, p.software
		FROM candidates c
		JOIN candidate_profiles p ON p.candidate_id = c.id
		WHERE c.id = ANY($1::text[]::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	texts := make(map[string]string, len(ids))
	for rows.Next() {
		var id, resume string
		var headline *string
		var profile []byte
		var certs, software []string
		if err := rows.Scan(&id, &resume, &headline, &profile, &certs, &software); err != nil {
			return nil, err
		}
		text := strings.TrimSpace(resume)
		if text == "" {
			text = profileText(headline, profile, certs, software)
		}
		texts[id] = truncate(text, maxRerankCandidateChars)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]aiclient.RerankCandidate, 0, len(ids))
	for _, id := range ids {
		if text := texts[id]; text != "" {
			out = append(out, aiclient.RerankCandidate{ID: id, Text: text})
		} else {
			logf("match: candidate %s has no resume or profile text to rerank (or was deleted); left out", id)
		}
	}
	return out, nil
}

// roleText is the rendering of the role the rerank reads: the title, the job
// description as the employer wrote it, and the requirements as the row
// holds them now, since those columns are what the API edits and what the
// hard filters just applied.
func (m *matcher) roleText(ctx context.Context, roleID string) (string, error) {
	var title, description string
	var company *string
	var must, nice []byte
	var certs, software []string
	var minYears *int
	err := m.pool.QueryRow(ctx, `
		SELECT title, company, description, must_haves, nice_to_haves, required_certifications, required_software, min_years_experience
		FROM roles WHERE id = $1`, roleID).Scan(&title, &company, &description, &must, &nice, &certs, &software, &minYears)
	if err != nil {
		return "", err
	}
	var head, tail strings.Builder
	fmt.Fprintf(&head, "Title: %s\n", title)
	if company != nil && strings.TrimSpace(*company) != "" {
		fmt.Fprintf(&head, "Company: %s\n", strings.TrimSpace(*company))
	}
	list := func(heading string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&tail, "\n%s:\n", heading)
		for _, item := range items {
			fmt.Fprintf(&tail, "- %s\n", item)
		}
	}
	list("Must-haves", stringList(must))
	list("Nice-to-haves", stringList(nice))
	if len(certs)+len(software) > 0 || (minYears != nil && *minYears > 0) {
		tail.WriteString("\nRequired, as taxonomy ids:\n")
		if len(certs) > 0 {
			fmt.Fprintf(&tail, "- certifications: %s\n", strings.Join(certs, ", "))
		}
		if len(software) > 0 {
			fmt.Fprintf(&tail, "- software: %s\n", strings.Join(software, ", "))
		}
		if minYears != nil && *minYears > 0 {
			fmt.Fprintf(&tail, "- minimum years of experience: %d\n", *minYears)
		}
	}
	// The description is the long part, so it is what gives way to the limit.
	room := maxRerankRoleChars - len([]rune(head.String())) - len([]rune(tail.String())) - 2
	body := truncate(strings.TrimSpace(description), max(room, 0))
	if body != "" {
		body = "\n" + body + "\n"
	}
	return truncate(strings.TrimSpace(head.String()+body+tail.String()), maxRerankRoleChars), nil
}

// truncate cuts s to at most n characters (code points, as the AI service counts them).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n]))
}
