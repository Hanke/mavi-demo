package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/dbtest"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rerankAI answers /rerank with a result per candidate sent, best first by
// levels (the must-have level each candidate's id maps to; 2 when it has
// none), or with status. requests is what it was asked, in order.
type rerankAI struct {
	levels   map[string]int
	status   int
	mu       sync.Mutex // batches arrive at once
	requests []aiclient.RerankRequest
}

func (f *rerankAI) client(t *testing.T) *aiclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req aiclient.RerankRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/rerank" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"detail":"nope"}`))
			return
		}
		out := aiclient.RerankResponse{Provider: "fake", RubricVersion: "1", Results: []aiclient.RerankResult{}}
		for _, c := range req.Candidates {
			level, ok := f.levels[c.ID]
			if !ok {
				level = 2
			}
			out.Results = append(out.Results, rerankResult(c.ID, level, c.Text))
		}
		sort.SliceStable(out.Results, func(i, j int) bool { return out.Results[i].Score > out.Results[j].Score })
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return aiclient.New(srv.URL)
}

// rerankResult scores a candidate level/4, quoting their own text.
func rerankResult(id string, level int, text string) aiclient.RerankResult {
	must := aiclient.OptionalDimensionScoreLevel(level)
	return aiclient.RerankResult{
		ID:      id,
		Score:   float32(level) / 4,
		Reasons: []string{"Has closed the books before."},
		Dimensions: aiclient.DimensionScores{
			MustHaveCoverage: aiclient.OptionalDimensionScore{Level: &must, Evidence: "Names the required licence.", Quotes: []string{text}},
			ExperienceDepth:  aiclient.DimensionScore{Level: aiclient.DimensionScoreLevel(level), Evidence: "Ran the month-end close.", Quotes: []string{text}},
			SoftwareFluency:  aiclient.OptionalDimensionScore{Evidence: "the role names none", Quotes: []string{}},
			IndustryFit:      aiclient.OptionalDimensionScore{Evidence: "the role names none", Quotes: []string{}},
			NiceToHaves:      aiclient.OptionalDimensionScore{Evidence: "the role states none", Quotes: []string{}},
		},
	}
}

// unit is a unit vector at degrees from unit(0), in pgvector's text form.
func unit(degrees float64) string {
	vec := make([]float32, EmbeddingDim)
	rad := degrees * math.Pi / 180
	vec[0], vec[1] = float32(math.Cos(rad)), float32(math.Sin(rad))
	return vectorLiteral(vec)
}

// matchPool is a database with one embedded role and the candidates added to it.
type matchPool struct {
	t    *testing.T
	pool *pgxpool.Pool
	role string
}

func newMatchPool(t *testing.T) *matchPool {
	t.Helper()
	old := logf
	logf = func(string, ...any) {}
	t.Cleanup(func() { logf = old })
	p := &matchPool{t: t, pool: dbtest.Pool(t)}
	p.scan(&p.role, `INSERT INTO roles (title, description, must_haves, embedding, embedding_model)
		VALUES ('Controller', 'Own the month-end close.', '["CPA licence"]', $1::vector, 'fake') RETURNING id::text`, unit(0))
	return p
}

func (p *matchPool) exec(sql string, args ...any) {
	p.t.Helper()
	if _, err := p.pool.Exec(context.Background(), sql, args...); err != nil {
		p.t.Fatal(err)
	}
}

func (p *matchPool) scan(dest any, sql string, args ...any) {
	p.t.Helper()
	if err := p.pool.QueryRow(context.Background(), sql, args...).Scan(dest); err != nil {
		p.t.Fatal(err)
	}
}

// add is a candidate who passes the role's filters, with a resume, embedded
// at degrees from the role (negative for a profile that is not embedded).
func (p *matchPool) add(name string, degrees float64) string {
	p.t.Helper()
	var id string
	p.scan(&id, `INSERT INTO candidates (full_name, resume_text) VALUES ($1, $2) RETURNING id::text`, name, name+" closed the books at Acme.")
	p.exec(`INSERT INTO candidate_profiles (candidate_id) VALUES ($1)`, id)
	if degrees >= 0 {
		p.exec(`UPDATE candidate_profiles SET embedding = $2::vector, embedding_model = 'fake' WHERE candidate_id = $1`, id, unit(degrees))
	}
	p.exec(`INSERT INTO candidate_availability (candidate_id, timezone, work_start, work_end, hours_per_week, available_from)
		VALUES ($1, 'America/Chicago', '09:00', '17:00', 40, '2026-01-01')`, id)
	return id
}

func (p *matchPool) handler(ai *rerankAI, cfg MatchConfig) jobs.Handler {
	p.t.Helper()
	tax, err := taxonomy.Load(dbtest.TaxonomyPath())
	if err != nil {
		p.t.Fatal(err)
	}
	return Registry(p.pool, ai.client(p.t), tax, cfg)[KindMatchRole]
}

func (p *matchPool) match(ai *rerankAI, cfg MatchConfig) {
	p.t.Helper()
	if err := p.handler(ai, cfg)(context.Background(), job(KindMatchRole, "role_id", p.role)); err != nil {
		p.t.Fatal(err)
	}
}

// matches is the role's matches as ops lists them: best first.
func (p *matchPool) matches() []store.Match {
	p.t.Helper()
	out, err := store.New(p.pool).ListMatches(context.Background(), store.MatchFilter{RoleID: p.role}, store.Page{})
	if err != nil {
		p.t.Fatal(err)
	}
	return out
}

func summary(matches []store.Match) string {
	parts := make([]string, len(matches))
	for i, m := range matches {
		parts[i] = m.CandidateName + " " + string(m.Status)
	}
	return strings.Join(parts, ", ")
}

// The whole run: only who passed the filters is reranked, every one of them
// becomes a match with the score, the explanation and the quotes, the top of
// the ranking is queued for review, and the employer sees none of it.
func TestMatchRoleWritesRankedMatches(t *testing.T) {
	p := newMatchPool(t)
	ctx := context.Background()
	ada, ben, cy := p.add("Ada", 10), p.add("Ben", 20), p.add("Cy", 30)
	var noHours string
	p.scan(&noHours, `INSERT INTO candidates (full_name, resume_text) VALUES ('Dee', 'Dee, CPA') RETURNING id::text`)
	p.exec(`INSERT INTO candidate_profiles (candidate_id, embedding, embedding_model) VALUES ($1, $2::vector, 'fake')`, noHours, unit(1))

	// The rerank disagrees with the embedding order: Cy is best, Ada worst.
	ai := &rerankAI{levels: map[string]int{cy: 4, ben: 3, ada: 1}}
	p.match(ai, MatchConfig{ReviewSize: 2})

	if len(ai.requests) != 1 {
		t.Fatalf("want one rerank call, got %d", len(ai.requests))
	}
	req := ai.requests[0]
	for _, want := range []string{"Title: Controller", "Own the month-end close.", "- CPA licence"} {
		if !strings.Contains(req.Role, want) {
			t.Errorf("role text lacks %q:\n%s", want, req.Role)
		}
	}
	// Nearest first, as retrieved, and nobody who failed a filter.
	if len(req.Candidates) != 3 || req.Candidates[0].ID != ada || req.Candidates[1].ID != ben || req.Candidates[2].ID != cy {
		t.Fatalf("reranked %+v", req.Candidates)
	}
	if req.Candidates[0].Text != "Ada closed the books at Acme." {
		t.Errorf("candidate text = %q", req.Candidates[0].Text)
	}

	got := p.matches()
	if s := summary(got); s != "Cy pending_review, Ben pending_review, Ada proposed" {
		t.Fatalf("matches: %s", s)
	}
	top := got[0]
	if top.Score != 1 || got[1].Score != 0.75 || got[2].Score != 0.25 {
		t.Errorf("scores = %v, %v, %v", top.Score, got[1].Score, got[2].Score)
	}
	want := "Has closed the books before.\n\n" +
		"Must-have coverage: full (4/4). Names the required licence.\n" +
		"Relevant experience depth: full (4/4). Ran the month-end close.\n" +
		"Software fluency: not scored for this role. the role names none\n" +
		"Industry fit: not scored for this role. the role names none\n" +
		"Nice-to-haves: not scored for this role. the role states none"
	if top.Explanation != want {
		t.Errorf("explanation:\n%s\nwant:\n%s", top.Explanation, want)
	}
	var b matchBreakdown
	if err := json.Unmarshal(top.Breakdown, &b); err != nil {
		t.Fatal(err)
	}
	quotes := b.Dimensions.MustHaveCoverage.Quotes
	if len(quotes) != 1 || quotes[0] != "Cy closed the books at Acme." {
		t.Errorf("quotes = %v", quotes)
	}
	if b.Rank != 1 || b.RubricVersion != "1" || b.Provider != "fake" || math.Abs(b.Similarity-math.Cos(30*math.Pi/180)) > 1e-6 {
		t.Errorf("breakdown = %s", top.Breakdown)
	}
	var matchedRun string
	p.scan(&matchedRun, `SELECT id::text FROM filter_runs WHERE role_id = $1 AND matched_at IS NOT NULL`, p.role)
	if b.FilterRunID != matchedRun {
		t.Errorf("breakdown names run %s, the run written is %s", b.FilterRunID, matchedRun)
	}

	// Nothing is released, so the employer's scope is empty.
	released, err := store.New(p.pool).ListMatches(ctx, store.MatchFilter{RoleID: p.role, ReleasedOnly: true}, store.Page{})
	if err != nil || len(released) != 0 {
		t.Fatalf("employer sees %d matches (%v)", len(released), err)
	}
}

// Running a role again replaces what the earlier run wrote: one row per
// candidate, new scores and queue, nobody the run no longer ranks. What ops
// decided or wrote by hand stays.
func TestMatchRoleAgainReplacesWithoutDuplicates(t *testing.T) {
	p := newMatchPool(t)
	ada, ben, cy, dan, eve := p.add("Ada", 10), p.add("Ben", 20), p.add("Cy", 30), p.add("Dan", 40), p.add("Eve", 50)
	ai := &rerankAI{levels: map[string]int{ada: 4, ben: 3, cy: 2, dan: 1, eve: 0}}
	p.match(ai, MatchConfig{ReviewSize: 2})
	if s := summary(p.matches()); s != "Ada pending_review, Ben pending_review, Cy proposed, Dan proposed, Eve proposed" {
		t.Fatalf("first run: %s", s)
	}

	// Ops rejects Ada. Ben and Dan leave the pool; Dan's match was released
	// and taken back, so it has review history. Ops proposes Fay by hand, who
	// never passes the filters.
	p.exec(`UPDATE matches SET status = 'rejected' WHERE candidate_id = $1`, ada)
	p.exec(`UPDATE candidates SET status = 'archived' WHERE id = ANY($1::text[]::uuid[])`, []string{ben, dan})
	p.exec(`UPDATE matches SET status = 'pending_review' WHERE candidate_id = $1`, dan)
	p.exec(`INSERT INTO review_events (match_id, action, actor) SELECT id, 'unrelease', 'ops' FROM matches WHERE candidate_id = $1`, dan)
	var fay string
	p.scan(&fay, `INSERT INTO candidates (full_name) VALUES ('Fay') RETURNING id::text`)
	p.exec(`INSERT INTO matches (role_id, candidate_id, score, status) VALUES ($1, $2, 0.1, 'pending_review')`, p.role, fay)

	ai.levels = map[string]int{eve: 3, cy: 2, ada: 0}
	p.match(ai, MatchConfig{ReviewSize: 2})

	got := p.matches()
	// Ada keeps the decision and the score it was made on. Ben is gone. Dan
	// cannot be deleted, so he is out of the queue. Fay is ops' own.
	if s := summary(got); s != "Ada rejected, Eve pending_review, Cy pending_review, Dan proposed, Fay pending_review" {
		t.Fatalf("second run: %s", s)
	}
	if got[0].Score != 1 || got[1].Score != 0.75 || got[2].Score != 0.5 {
		t.Errorf("scores = %v, %v, %v", got[0].Score, got[1].Score, got[2].Score)
	}
	var runs, written int
	p.scan(&runs, `SELECT count(*) FROM filter_runs WHERE role_id = $1`, p.role)
	p.scan(&written, `SELECT count(*) FROM filter_runs WHERE role_id = $1 AND matched_at IS NOT NULL`, p.role)
	if runs != 2 || written != 2 {
		t.Errorf("%d runs, %d written", runs, written)
	}

	// A run that ranks nobody clears what the runs wrote and nothing else.
	p.exec(`UPDATE candidates SET status = 'archived' WHERE id = ANY($1::text[]::uuid[])`, []string{ada, cy, eve})
	p.match(ai, MatchConfig{})
	if s := summary(p.matches()); s != "Ada rejected, Dan proposed, Fay pending_review" {
		t.Fatalf("empty run: %s", s)
	}
	if len(ai.requests) != 2 {
		t.Errorf("an empty shortlist was sent to the rerank (%d calls)", len(ai.requests))
	}
}

// queued is how many match_role jobs wait for the role, and how far ahead
// the first of them runs.
func (p *matchPool) queued() (n int, in time.Duration) {
	p.t.Helper()
	var seconds float64
	if err := p.pool.QueryRow(context.Background(), `
		SELECT count(*), coalesce(extract(epoch FROM min(run_at) - now()), 0) FROM jobs
		WHERE kind = $1 AND status = 'queued' AND payload->>'role_id' = $2`, KindMatchRole, p.role).Scan(&n, &seconds); err != nil {
		p.t.Fatal(err)
	}
	return n, time.Duration(seconds * float64(time.Second))
}

func (p *matchPool) count(sql string, args ...any) (n int) {
	p.t.Helper()
	p.scan(&n, sql, args...)
	return n
}

// What cannot be judged yet is never written as an empty result. With no job
// on its way to embed what is missing, the attempt fails; with one, the run
// steps aside for a later one, however long the embedding takes.
func TestMatchRoleWaitsForEmbeddings(t *testing.T) {
	p := newMatchPool(t)
	ctx := context.Background()
	ada := p.add("Ada", -1)
	ai := &rerankAI{}
	run := p.handler(ai, MatchConfig{})
	this := job(KindMatchRole, "role_id", p.role)

	err := run(ctx, this)
	if err == nil || errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "none has an embedding") {
		t.Fatalf("unembedded profiles, nothing pending: %v", err)
	}
	p.exec(`INSERT INTO jobs (kind, payload) VALUES ($1, jsonb_build_object('candidate_id', $2::text))`, KindEmbedProfile, ada)
	if err := run(ctx, this); err != nil {
		t.Fatalf("unembedded profiles, embedding pending: %v", err)
	}
	if n, in := p.queued(); n != 1 || in < matchRetryDelay-5*time.Second || in > matchRetryDelay {
		t.Fatalf("want one run queued %s ahead, got %d in %s", matchRetryDelay, n, in)
	}
	// Waiting again does not pile up runs.
	if err := run(ctx, this); err != nil {
		t.Fatal(err)
	}
	if n, _ := p.queued(); n != 1 {
		t.Fatalf("%d runs queued", n)
	}
	p.exec(`DELETE FROM jobs`)

	p.exec(`UPDATE roles SET embedding = NULL, embedding_model = NULL WHERE id = $1`, p.role)
	runs := p.count(`SELECT count(*) FROM filter_runs`)
	err = run(ctx, this)
	if err == nil || errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "not embedded yet") {
		t.Fatalf("unembedded role, nothing pending: %v", err)
	}
	p.exec(`INSERT INTO jobs (kind, payload, status) VALUES ($1, jsonb_build_object('role_id', $2::text), 'running')`, KindEmbedRole, p.role)
	if err := run(ctx, this); err != nil {
		t.Fatalf("unembedded role, embedding pending: %v", err)
	}
	if n, _ := p.queued(); n != 1 {
		t.Fatalf("%d runs queued", n)
	}
	// A role that is not ready is not filtered either: no run is recorded.
	if got := p.count(`SELECT count(*) FROM filter_runs`); got != runs {
		t.Errorf("waiting for the role recorded %d filter runs", got-runs)
	}
	if len(ai.requests) != 0 || len(p.matches()) != 0 {
		t.Fatalf("%d rerank calls, %d matches", len(ai.requests), len(p.matches()))
	}
}

// A candidate who passes the filters but is waiting for an embedding was not
// judged, so a run that cannot rank them leaves their match alone and comes
// back for them.
func TestMatchRoleKeepsTheMatchOfACandidateStillBeingEmbedded(t *testing.T) {
	p := newMatchPool(t)
	ada, ben := p.add("Ada", 10), p.add("Ben", 20)
	ai := &rerankAI{levels: map[string]int{ada: 4, ben: 3}}
	p.match(ai, MatchConfig{})
	if n, _ := p.queued(); n != 0 {
		t.Fatalf("a complete run queued %d more", n)
	}

	// Ada edits her profile: no embedding until her embed_profile job runs.
	p.exec(`UPDATE candidate_profiles SET embedding = NULL, embedding_model = NULL WHERE candidate_id = $1`, ada)
	p.exec(`INSERT INTO jobs (kind, payload) VALUES ($1, jsonb_build_object('candidate_id', $2::text))`, KindEmbedProfile, ada)
	p.match(ai, MatchConfig{})

	if s := summary(p.matches()); s != "Ada pending_review, Ben pending_review" {
		t.Fatalf("matches: %s", s)
	}
	if got := ai.requests[len(ai.requests)-1].Candidates; len(got) != 1 || got[0].ID != ben {
		t.Fatalf("second run reranked %+v", got)
	}
	if n, in := p.queued(); n != 1 || in < matchPickupDelay-5*time.Second {
		t.Fatalf("want one run queued %s ahead to pick Ada up, got %d in %s", matchPickupDelay, n, in)
	}
}

// What ops wrote by hand is ops': a run that ranks the same candidate does
// not rewrite it, so no later run takes it for its own and removes it.
func TestMatchRoleLeavesAHandWrittenMatchAlone(t *testing.T) {
	p := newMatchPool(t)
	ada, ben := p.add("Ada", 10), p.add("Ben", 20)
	p.exec(`INSERT INTO matches (role_id, candidate_id, score, explanation) VALUES ($1, $2, 0.1, 'Met her at the conference.')`, p.role, ada)
	ai := &rerankAI{levels: map[string]int{ada: 4, ben: 3}}
	p.match(ai, MatchConfig{})

	got := p.matches()
	if s := summary(got); s != "Ben pending_review, Ada proposed" {
		t.Fatalf("matches: %s", s)
	}
	if got[1].Score != 0.1 || got[1].Explanation != "Met her at the conference." || string(got[1].Breakdown) != "{}" {
		t.Fatalf("the hand-written match was rewritten: %+v", got[1])
	}
	p.exec(`UPDATE candidates SET status = 'archived' WHERE id = $1`, ada)
	p.match(ai, MatchConfig{})
	if s := summary(p.matches()); s != "Ben pending_review, Ada proposed" {
		t.Fatalf("after Ada left the pool: %s", s)
	}
}

// A job for a role that was filled or closed while it waited does nothing.
func TestMatchRoleSkipsARoleThatIsNotOpen(t *testing.T) {
	p := newMatchPool(t)
	ada := p.add("Ada", 10)
	ai := &rerankAI{levels: map[string]int{ada: 4}}
	p.match(ai, MatchConfig{})
	for _, status := range []string{"filled", "closed"} {
		p.exec(`UPDATE roles SET status = $2 WHERE id = $1`, p.role, status)
		p.match(ai, MatchConfig{})
	}
	if len(ai.requests) != 1 || p.count(`SELECT count(*) FROM filter_runs`) != 1 {
		t.Fatalf("a role that is not open was matched: %d rerank calls", len(ai.requests))
	}
	if s := summary(p.matches()); s != "Ada pending_review" {
		t.Fatalf("matches: %s", s)
	}
}

// A shortlist nobody can be read from is not a ranking of nobody: the earlier
// matches stay and the attempt is retried.
func TestMatchRoleRetriesAShortlistWithNothingToRead(t *testing.T) {
	p := newMatchPool(t)
	ctx := context.Background()
	ada := p.add("Ada", 10)
	ai := &rerankAI{levels: map[string]int{ada: 4}}
	p.match(ai, MatchConfig{})

	p.exec(`UPDATE candidates SET resume_text = '' WHERE id = $1`, ada)
	err := p.handler(ai, MatchConfig{})(ctx, job(KindMatchRole, "role_id", p.role))
	if err == nil || errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "can be read") {
		t.Fatalf("unreadable shortlist: %v", err)
	}
	if s := summary(p.matches()); s != "Ada pending_review" || len(ai.requests) != 1 {
		t.Fatalf("matches: %s, %d rerank calls", s, len(ai.requests))
	}
}

// A long shortlist is reranked in batches and merged into one ranking.
func TestMatchRoleReranksInBatches(t *testing.T) {
	p := newMatchPool(t)
	ai := &rerankAI{levels: map[string]int{}}
	names := []string{"Ada", "Ben", "Cy", "Dan", "Eve", "Fay", "Gus", "Hal", "Ida", "Jo", "Kit", "Lea"}
	for i, name := range names {
		ai.levels[p.add(name, float64(i+1))] = i % 5 // the last batch holds a 0 and a 1
	}
	last := p.add("Max", 60)
	ai.levels[last] = 4
	p.match(ai, MatchConfig{ReviewSize: 3})

	sizes := []int{}
	for _, req := range ai.requests {
		sizes = append(sizes, len(req.Candidates))
	}
	sort.Ints(sizes)
	if len(sizes) != 2 || sizes[0] != 3 || sizes[1] != rerankBatchSize {
		t.Fatalf("batches of %v", sizes)
	}
	got := p.matches()
	if len(got) != 13 {
		t.Fatalf("%d matches", len(got))
	}
	pending := 0
	for i, m := range got {
		if i > 0 && m.Score > got[i-1].Score {
			t.Fatalf("not in order of score: %s", summary(got))
		}
		if m.Status == "pending_review" {
			pending++
			if m.Score != 1 {
				t.Errorf("%s is in the queue with %v", m.CandidateName, m.Score)
			}
		}
	}
	// The three 4s: Eve and Jo from the first batch, Max from the second.
	if pending != 3 || got[2].Score != 1 || got[3].Score != 0.75 {
		t.Fatalf("merged ranking: %s", summary(got))
	}
}

func TestBatchesKeepToTheirLimits(t *testing.T) {
	long := strings.Repeat("é", rerankBatchChars/2+1)
	in := []aiclient.RerankCandidate{{ID: "a", Text: long}, {ID: "b", Text: long}, {ID: "c", Text: "short"}}
	got := batches(in)
	if len(got) != 2 || len(got[0]) != 1 || got[0][0].ID != "a" || len(got[1]) != 2 || got[1][1].ID != "c" {
		t.Fatalf("by characters: %d batches", len(got))
	}
	many := make([]aiclient.RerankCandidate, 2*rerankBatchSize+1)
	if got := batches(many); len(got) != 3 || len(got[0]) != rerankBatchSize || len(got[2]) != 1 {
		t.Fatalf("by count: %d batches", len(got))
	}
	if got := batches(nil); len(got) != 0 {
		t.Fatalf("nobody: %d batches", len(got))
	}
}

func TestMatchRoleFailuresAreClassified(t *testing.T) {
	p := newMatchPool(t)
	ctx := context.Background()
	p.add("Ada", 10)

	// The service will never take this request; the model had a bad day.
	err := p.handler(&rerankAI{status: http.StatusUnprocessableEntity}, MatchConfig{})(ctx, job(KindMatchRole, "role_id", p.role))
	if !errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("422: %v", err)
	}
	err = p.handler(&rerankAI{status: http.StatusBadGateway}, MatchConfig{})(ctx, job(KindMatchRole, "role_id", p.role))
	if err == nil || errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("502: %v", err)
	}
	if n := len(p.matches()); n != 0 {
		t.Fatalf("a failed rerank wrote %d matches", n)
	}

	run := p.handler(&rerankAI{}, MatchConfig{})
	if err := run(ctx, job(KindMatchRole, "role_id", "00000000-0000-0000-0000-000000000000")); err != nil {
		t.Fatalf("a deleted role is done, not failed: %v", err)
	}
	if err := run(ctx, job(KindMatchRole, "role_id", "not-a-uuid")); !errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("bad id: %v", err)
	}
	if err := run(ctx, jobs.Job{ID: 1, Kind: KindMatchRole, Payload: json.RawMessage(`{}`)}); !errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("empty payload: %v", err)
	}
}

// A run that started before one already written does not overwrite it.
func TestReplaceRunMatchesRefusesAnOlderRun(t *testing.T) {
	p := newMatchPool(t)
	ctx := context.Background()
	ada := p.add("Ada", 10)
	st := store.New(p.pool)
	tax, err := taxonomy.Load(dbtest.TaxonomyPath())
	if err != nil {
		t.Fatal(err)
	}
	older, err := HardFilter(ctx, st, tax, p.role, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := HardFilter(ctx, st, tax, p.role, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	ranked := func(score float64) []store.RankedMatch {
		return []store.RankedMatch{{CandidateID: ada, Score: score, Breakdown: json.RawMessage(`{"filter_run_id":"x"}`)}}
	}
	if _, err := st.ReplaceRunMatches(ctx, newer.ID, ranked(0.9), 5); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceRunMatches(ctx, older.ID, ranked(0.1), 5); !errors.Is(err, store.ErrSuperseded) {
		t.Fatalf("older run: %v", err)
	}
	if got := p.matches(); len(got) != 1 || got[0].Score != 0.9 {
		t.Fatalf("matches after the older run: %+v", got)
	}
	// The same run again (a retried write) is not older than itself.
	if res, err := st.ReplaceRunMatches(ctx, newer.ID, ranked(0.8), 5); err != nil || res.Written != 1 || res.PendingReview != 1 {
		t.Fatalf("same run again: %+v, %v", res, err)
	}
	if _, err := st.ReplaceRunMatches(ctx, "00000000-0000-0000-0000-000000000000", nil, 5); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing run: %v", err)
	}

	// A candidate deleted since the run takes no place in the queue.
	gone := store.RankedMatch{CandidateID: "00000000-0000-0000-0000-000000000001", Score: 1, Breakdown: json.RawMessage(`{"filter_run_id":"x"}`)}
	res, err := st.ReplaceRunMatches(ctx, newer.ID, append([]store.RankedMatch{gone}, ranked(0.8)...), 1)
	if err != nil || res.Written != 1 || res.PendingReview != 1 {
		t.Fatalf("with a deleted candidate first: %+v, %v", res, err)
	}
}

func TestRerankTextIsCutToTheServiceLimits(t *testing.T) {
	if got := truncate("héllo wörld", 5); got != "héllo" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("short", 20); got != "short" {
		t.Errorf("truncate = %q", got)
	}
	p := newMatchPool(t)
	p.exec(`UPDATE roles SET description = $2 WHERE id = $1`, p.role, strings.Repeat("é", maxRerankRoleChars+10))
	p.exec(`UPDATE roles SET embedding = $2::vector WHERE id = $1`, p.role, unit(0)) // the edit above is not through the store, so nothing cleared it
	id := p.add("Ada", 10)
	p.exec(`UPDATE candidates SET resume_text = $2 WHERE id = $1`, id, strings.Repeat("é", maxRerankCandidateChars+10))
	ai := &rerankAI{}
	p.match(ai, MatchConfig{})
	req := ai.requests[0]
	if n := len([]rune(req.Role)); n > maxRerankRoleChars || !strings.Contains(req.Role, "- CPA licence") {
		t.Errorf("role text is %d characters, or lost its must-haves", n)
	}
	if n := len([]rune(req.Candidates[0].Text)); n != maxRerankCandidateChars {
		t.Errorf("candidate text is %d characters", n)
	}
}
