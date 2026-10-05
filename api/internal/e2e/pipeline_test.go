package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/dbtest"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/colehanke/mavi-demo/api/internal/server"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/tasks"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
)

// person is one candidate of the pool: what their profile holds, whether
// they have said when they can work, and what the mock LLM makes of them.
type person struct {
	name      string
	certs     []string
	software  []string
	years     int
	available bool
	score     float64 // the mock LLM's, if it is ever asked
}

// The job description the employer pastes, and what the mock LLM extracts
// from it: a US CPA, NetSuite and five years are the must-haves.
const (
	pipelineJD = "Senior Accountant at Northwind Traders.\nActive CPA required. 5+ years. NetSuite daily. Full-time, Central time."

	pipelineRequirements = `{"title":"Senior Accountant","required_certifications":["cpa_us"],"required_software":["netsuite"],
		"min_years_experience":5,"must_haves":["Active CPA","5+ years of experience","NetSuite as a daily user"],"nice_to_haves":[],
		"timezone":"America/Chicago","min_overlap_hours":4,"hours_per_week":40,"starts_on":"2026-11-02"}`
)

// pipelinePool is who is in the pool when the role arrives. Four meet every
// must-have, and the LLM finds three of them good enough to put forward.
// Each of the other four misses exactly one must-have; the LLM would rate
// them highly, and the hard filters see to it that it is never asked.
var pipelinePool = []person{
	{name: "Ada Okafor", certs: []string{"cpa_us"}, software: []string{"NetSuite", "Excel"}, years: 9, available: true, score: 0.95},
	{name: "Ben Larsen", certs: []string{"cpa_us"}, software: []string{"NetSuite"}, years: 7, available: true, score: 0.85},
	{name: "Cy Tran", certs: []string{"cpa_us", "CMA"}, software: []string{"NetSuite", "BlackLine"}, years: 6, available: true, score: 0.7},
	{name: "Dan Reyes", certs: []string{"cpa_us"}, software: []string{"NetSuite"}, years: 5, available: true, score: 0.4},

	{name: "Eve Stone", certs: nil, software: []string{"NetSuite"}, years: 8, available: true, score: 0.9},                   // no CPA
	{name: "Fay Novak", certs: []string{"cpa_us"}, software: []string{"QuickBooks"}, years: 8, available: true, score: 0.9},  // no NetSuite
	{name: "Gus Meyer", certs: []string{"cpa_us"}, software: []string{"NetSuite"}, years: 2, available: true, score: 0.9},    // too junior
	{name: "Hal Brandt", certs: []string{"cpa_us"}, software: []string{"NetSuite"}, years: 12, available: false, score: 0.9}, // never said when he can work
}

// resume is the text the rerank reads. Its first line is the name, which is
// how the mock LLM knows whom it is judging.
func (p person) resume() string {
	return fmt.Sprintf("%s\nAccountant with %d years of experience. %s. %s.", p.name, p.years, strings.Join(p.certs, ", "), strings.Join(p.software, ", "))
}

// One role from the employer's job description to the two profiles the
// employer is shown, with every stage in between real except the model: the
// HTTP API, the job queue and its worker, the hard filters and the vector
// retrieval in Postgres, the review decisions and the release.
func TestPipelineFromIntakeToRelease(t *testing.T) {
	captureLogs(t)
	llm := newMockAI(t, pipelineRequirements, pipelinePool)
	c := newStack(t, llm.url)

	// The pool. Ops adds each candidate with a profile, which the worker
	// embeds; the candidate says when they can work, or does not.
	ids := map[string]string{}
	for _, p := range pipelinePool {
		cand := send[contract.Candidate](c, ops, "POST", "/candidates", map[string]any{"full_name": p.name, "resume_text": p.resume()}, 201)
		ids[p.name] = cand.ID
		send[contract.Profile](c, ops, "PUT", "/candidates/"+cand.ID+"/profile", map[string]any{
			"headline": "Accountant", "years_experience": p.years, "certifications": p.certs, "software": p.software,
		}, 201)
		if p.available {
			send[contract.WorkAvailability](c, talent(cand.ID), "PUT", "/candidates/"+cand.ID+"/availability", map[string]any{
				"timezone": "America/New_York", "work_start": "09:00", "work_end": "17:00", "hours_per_week": 40, "available_from": "2026-01-01",
			}, 201)
		}
	}
	for name, id := range ids {
		c.eventually("the embedding of "+name+"'s profile", 15*time.Second, func() (bool, string) {
			return send[contract.Profile](c, ops, "GET", "/candidates/"+id+"/profile", nil, 200).EmbeddedAt != nil, "not embedded"
		})
	}

	// The employer pastes the job description. The answer is the role as the
	// LLM read it and the matching run queued for it, which is the pipeline.
	intake := send[contract.RoleIntake](c, employer, "POST", "/roles/intake", map[string]any{"description": pipelineJD}, 201)
	role := intake.Role.ID
	if got := intake.Role.RequiredCertifications; !slices.Equal(got, []string{"cpa_us"}) || intake.Role.EmbeddedAt == nil {
		t.Fatalf("the role requires %v and was embedded at %v", got, intake.Role.EmbeddedAt)
	}
	c.eventually("the matching run", 15*time.Second, func() (bool, string) {
		return c.finished(send[contract.Job](c, ops, "GET", fmt.Sprintf("/jobs/%d", intake.MatchingJob.ID), nil, 200))
	})

	// The hard filters dropped one candidate each, in the order they are
	// applied, and the LLM was asked about the four who were left and nobody
	// else.
	status := send[contract.RoleMatchStatus](c, ops, "GET", "/roles/"+role+"/match-status", nil, 200)
	if status.Status != contract.RoleMatchStateInReview || status.Run == nil || status.Run.MatchStatus == nil || *status.Run.MatchStatus != contract.RunStatusMatched {
		t.Fatalf("match status for ops: %+v", status)
	}
	if got, want := funnel(*status.Run), "pool 8: profile 8, certifications 7, software 6, experience 5, availability 4, timezone_overlap 4"; got != want {
		t.Errorf("funnel:\n got %s\nwant %s", got, want)
	}
	if got := llm.asked(); got != "Ada Okafor, Ben Larsen, Cy Tran, Dan Reyes" {
		t.Errorf("the LLM was asked to rank: %s", got)
	}

	// They are the role's matches, best first. The three the LLM scored at or
	// above the minimum are the review queue; Dan is on record and not in it,
	// and nobody is in reserve for a swap.
	matches := send[[]contract.Match](c, ops, "GET", "/matches?role_id="+role, nil, 200)
	if got := summary(matches); got != "Ada Okafor 0.95 pending_review, Ben Larsen 0.85 pending_review, Cy Tran 0.7 pending_review, Dan Reyes 0.4 proposed" {
		t.Fatalf("matches: %s", got)
	}
	queue := send[contract.ReviewQueue](c, ops, "GET", "/roles/"+role+"/review-queue", nil, 200)
	if names(queue.Pending) != "Ada Okafor, Ben Larsen, Cy Tran" || len(queue.Approved) != 0 || queue.Next != nil {
		t.Fatalf("review queue: pending %q, approved %q, next %+v", names(queue.Pending), names(queue.Approved), queue.Next)
	}
	match := map[string]string{} // candidate name -> match id
	for _, m := range matches {
		match[m.CandidateName] = m.ID
	}

	// None of it has reached the employer.
	if seen := send[[]contract.Match](c, employer, "GET", "/matches?role_id="+role, nil, 200); len(seen) != 0 {
		t.Fatalf("the employer sees %q before anything is released", names(seen))
	}
	if s := send[contract.RoleMatchStatus](c, employer, "GET", "/roles/"+role+"/match-status", nil, 200); s.Status != contract.RoleMatchStateInReview || s.Run != nil {
		t.Fatalf("match status for the employer: %+v", s)
	}

	// Ops approves two: the best, and Cy rather than Ben, so what is released
	// is what ops chose and not the top of the ranking. One approval is not
	// enough to release, and still shows the employer nothing.
	send[contract.Match](c, ops, "POST", "/matches/"+match["Ada Okafor"]+"/approve", nil, 200)
	send[contract.Error](c, ops, "POST", "/roles/"+role+"/release", nil, 409)
	if seen := send[[]contract.Match](c, employer, "GET", "/matches?role_id="+role, nil, 200); len(seen) != 0 {
		t.Fatalf("the employer sees %q after one approval", names(seen))
	}
	send[contract.Match](c, ops, "POST", "/matches/"+match["Cy Tran"]+"/approve", map[string]any{"reason": "ran a multi-entity close"}, 200)
	if released := send[[]contract.Match](c, ops, "POST", "/roles/"+role+"/release", nil, 200); names(released) != "Ada Okafor, Cy Tran" {
		t.Fatalf("released: %s", names(released))
	}

	// The employer sees those two and nobody else, however they ask.
	seen := send[[]contract.Match](c, employer, "GET", "/matches?role_id="+role, nil, 200)
	if names(seen) != "Ada Okafor, Cy Tran" {
		t.Fatalf("the employer sees %q, want the two ops approved", names(seen))
	}
	for _, m := range seen {
		if m.ReleasedAt == nil || m.Status != contract.MatchStatusApproved {
			t.Errorf("the employer was shown %s's match as %s, released at %v", m.CandidateName, m.Status, m.ReleasedAt)
		}
	}
	if all := send[[]contract.Match](c, employer, "GET", "/matches", nil, 200); names(all) != "Ada Okafor, Cy Tran" {
		t.Errorf("the employer's whole list is %q", names(all))
	}
	send[contract.Match](c, employer, "GET", "/matches/"+match["Ada Okafor"], nil, 200)
	for _, name := range []string{"Ben Larsen", "Dan Reyes"} {
		send[contract.Error](c, employer, "GET", "/matches/"+match[name], nil, 404)
	}
	if s := send[contract.RoleMatchStatus](c, employer, "GET", "/roles/"+role+"/match-status", nil, 200); s.Status != contract.RoleMatchStateReady || s.Released != 2 {
		t.Errorf("match status for the employer after the release: %+v", s)
	}

	// Ben is still waiting for a decision, and the four who failed a filter
	// were never matched at all.
	queue = send[contract.ReviewQueue](c, ops, "GET", "/roles/"+role+"/review-queue", nil, 200)
	if names(queue.Pending) != "Ben Larsen" || names(queue.Approved) != "Ada Okafor, Cy Tran" {
		t.Errorf("review queue after the release: pending %q, approved %q", names(queue.Pending), names(queue.Approved))
	}
	for _, name := range []string{"Eve Stone", "Fay Novak", "Gus Meyer", "Hal Brandt"} {
		if got := send[[]contract.Match](c, ops, "GET", "/matches?candidate_id="+ids[name], nil, 200); len(got) != 0 {
			t.Errorf("%s failed a hard filter and has %d matches", name, len(got))
		}
	}
}

// summary is a list of matches as "name score status", in order.
func summary(matches []contract.Match) string {
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = fmt.Sprintf("%s %g %s", m.CandidateName, m.Score, m.Status)
	}
	return strings.Join(out, ", ")
}

// funnel is how a run's hard filters narrowed the pool, in the order applied.
func funnel(run contract.FilterRun) string {
	stages := make([]string, len(run.Stages))
	for i, s := range run.Stages {
		stages[i] = fmt.Sprintf("%s %d", s.Filter, s.Remaining)
	}
	return fmt.Sprintf("pool %d: %s", run.Pool, strings.Join(stages, ", "))
}

// newStack is the API as cmd/api runs it, on a database of its own: the HTTP
// server and a worker for its job queue, both calling the AI service at
// aiURL, with the matching run at its default sizes.
func newStack(t *testing.T, aiURL string) *client {
	t.Helper()
	pool := dbtest.Pool(t)
	tax, err := taxonomy.Load(dbtest.TaxonomyPath())
	if err != nil {
		t.Fatal(err)
	}
	ai := aiclient.New(aiURL)
	handlers := tasks.Registry(pool, ai, tax, tasks.MatchConfig{})
	api := httptest.NewServer(server.New(server.Config{
		DB:       pool,
		AI:       ai,
		Store:    store.New(pool),
		Taxonomy: tax,
		Jobs:     jobs.NewQueue(pool),
		JobKinds: handlers.Kinds(),
		EmbedRole: func(ctx context.Context, roleID string) error {
			return tasks.EmbedRole(ctx, pool, ai, roleID)
		},
		CORSOrigin: "*",
	}))
	t.Cleanup(api.Close)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	worker := jobs.NewWorker(pool, handlers, jobs.WorkerConfig{
		Concurrency:   2,
		PollInterval:  10 * time.Millisecond,
		ShutdownGrace: 100 * time.Millisecond,
		Backoff:       func(int) time.Duration { return 50 * time.Millisecond },
	})
	go func() {
		defer close(stopped)
		if err := worker.Run(ctx); err != nil {
			t.Errorf("worker: %v", err)
		}
	}()
	// Registered after the pool, so the worker is gone before the pool closes.
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return newClient(t, api.URL)
}

// captureLogs keeps what the server and the worker log out of the test's
// output, and prints it if the test fails. It goes first in a test, so that
// it is undone last, when nothing is left running to write a line.
func captureLogs(t *testing.T) {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(old)
		if t.Failed() {
			t.Logf("server and worker log:\n%s", buf.String())
		}
	})
}

// mockAI stands in for the AI service, and so for the LLM behind it, with
// scripted answers: /parse-jd reads every job description as requirements,
// and /rerank gives each candidate the score the script has for the name on
// the first line of their text. Embeddings are worked out from the words of
// the input, so retrieval compares real vectors.
type mockAI struct {
	t            *testing.T
	url          string
	requirements string
	scores       map[string]float64

	mu       sync.Mutex
	reranked []string // the names /rerank was asked about
}

func newMockAI(t *testing.T, requirements string, pool []person) *mockAI {
	t.Helper()
	m := &mockAI{t: t, requirements: requirements, scores: map[string]float64{}}
	for _, p := range pool {
		m.scores[p.name] = p.score
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		answer(w, aiclient.HealthResponse{Status: "ok", LlmProvider: "mock", EmbeddingProvider: "mock"})
	})
	mux.HandleFunc("POST /parse-jd", func(w http.ResponseWriter, r *http.Request) {
		answer(w, map[string]any{"company": "Northwind Traders", "requirements": json.RawMessage(m.requirements), "provider": "mock"})
	})
	mux.HandleFunc("POST /embed-batch", m.embedBatch)
	mux.HandleFunc("POST /rerank", m.rerank)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the mock AI service has no answer for %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m.url = srv.URL
	return m
}

// asked is the names of everyone /rerank was asked about, sorted.
func (m *mockAI) asked() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Clone(m.reranked)
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func (m *mockAI) rerank(w http.ResponseWriter, r *http.Request) {
	var req aiclient.RerankRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	out := aiclient.RerankResponse{Provider: "mock", RubricVersion: "1", Results: []aiclient.RerankResult{}}
	for _, cand := range req.Candidates {
		name, _, _ := strings.Cut(cand.Text, "\n")
		score, ok := m.scores[name]
		if !ok {
			m.t.Errorf("the mock LLM was asked about %q, who is not in its script", name)
		}
		m.mu.Lock()
		m.reranked = append(m.reranked, name)
		m.mu.Unlock()

		level := int(math.Round(score * 4))
		must := aiclient.OptionalDimensionScoreLevel(level)
		out.Results = append(out.Results, aiclient.RerankResult{
			ID:      cand.ID,
			Score:   float32(score),
			Reasons: []string{"Scripted for " + name + "."},
			Dimensions: aiclient.DimensionScores{
				MustHaveCoverage: aiclient.OptionalDimensionScore{Level: &must, Evidence: "Names the licence and the system.", Quotes: []string{name}},
				ExperienceDepth:  aiclient.DimensionScore{Level: aiclient.DimensionScoreLevel(level), Evidence: "Has done the work.", Quotes: []string{name}},
				SoftwareFluency:  aiclient.OptionalDimensionScore{Evidence: "not scripted", Quotes: []string{}},
				IndustryFit:      aiclient.OptionalDimensionScore{Evidence: "not scripted", Quotes: []string{}},
				NiceToHaves:      aiclient.OptionalDimensionScore{Evidence: "the role states none", Quotes: []string{}},
			},
		})
	}
	sort.SliceStable(out.Results, func(i, j int) bool { return out.Results[i].Score > out.Results[j].Score })
	answer(w, out)
}

func (m *mockAI) embedBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Inputs []aiclient.EmbedItem `json:"inputs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	out := aiclient.EmbedBatchResponse{Dim: tasks.EmbeddingDim, Provider: "mock"}
	for _, in := range req.Inputs {
		text := in.Text + string(in.Profile) + string(in.Requirements)
		out.Texts = append(out.Texts, text)
		out.Embeddings = append(out.Embeddings, embedding(text))
	}
	answer(w, out)
}

// embedding is a unit vector that is nearer to those of texts sharing its
// words: each word adds to one dimension, picked by its hash.
func embedding(text string) []float32 {
	vec := make([]float32, tasks.EmbeddingDim)
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for _, word := range words {
		h := fnv.New32a()
		h.Write([]byte(word))
		vec[h.Sum32()%uint32(len(vec))]++
	}
	var sum float64
	for _, x := range vec {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		vec[0] = 1
		return vec
	}
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / math.Sqrt(sum))
	}
	return vec
}

func answer(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
