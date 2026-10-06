package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/dbtest"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
	"github.com/jackc/pgx/v5/pgxpool"
)

const parsedProfile = `{"headline":" Senior Accountant, CPA ","years_experience":8,"positions":[{"title":"Senior Accountant","employer":"Acme"}],
	"certifications":["cpa_us"],"other_certifications":[],"software":["netsuite","quickbooks","netsuite"],"skills":["month-end close"],
	"availability":"two_weeks","available_from":"2026-11-02","timezone":"America/Chicago"}`

// intakeAI answers /parse-resume with profile (or parseStatus) and
// /embed-batch with a vector (or embedStatus). calls records the paths in
// order; parsed is the last text it was asked to parse.
type intakeAI struct {
	profile     string
	parseStatus int
	embedStatus int
	during      func() // runs while the "model" is parsing, before it answers
	parsed      atomic.Pointer[string]
	calls       []string
}

func (f *intakeAI) client(t *testing.T) *aiclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls = append(f.calls, r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		status := f.embedStatus
		answer := func() any {
			vec := make([]float32, EmbeddingDim)
			vec[0] = 1
			return aiclient.EmbedBatchResponse{Embeddings: [][]float32{vec}, Texts: []string{"x"}, Dim: EmbeddingDim, Provider: "fake"}
		}
		if r.URL.Path == "/parse-resume" {
			var req aiclient.ParseResumeRequest
			_ = json.Unmarshal(raw, &req)
			f.parsed.Store(&req.Text)
			if f.during != nil {
				f.during()
			}
			profile := f.profile
			if profile == "" {
				profile = parsedProfile
			}
			status = f.parseStatus
			answer = func() any {
				return map[string]any{"contact": map[string]any{"full_name": "Ada Okafor"}, "profile": json.RawMessage(profile), "provider": "fake"}
			}
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"nope"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(answer())
	}))
	t.Cleanup(srv.Close)
	return aiclient.New(srv.URL)
}

func intake(t *testing.T, pool *pgxpool.Pool, ai *intakeAI) jobs.Handler {
	t.Helper()
	tax, err := taxonomy.Load(dbtest.TaxonomyPath())
	if err != nil {
		t.Fatal(err)
	}
	return Registry(pool, ai.client(t), tax, MatchConfig{})[KindParseResume]
}

func newCandidate(t *testing.T, pool *pgxpool.Pool, resume string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO candidates (full_name, resume_text) VALUES ('Ada Okafor', $1) RETURNING id::text`, resume).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func profiles(t *testing.T, pool *pgxpool.Pool, candidate string) (n int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM candidate_profiles WHERE candidate_id = $1`, candidate).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The whole intake job: parse the stored text, write the profile with its
// hard-filter columns, embed it.
func TestParseResumeWritesProfileAndEmbedding(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	ai := &intakeAI{}
	handler := intake(t, pool, ai)
	cand := newCandidate(t, pool, "Ada Okafor\nSenior Accountant, CPA")

	if err := handler(ctx, job(KindParseResume, "candidate_id", cand)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ai.calls, " "); got != "/parse-resume /embed-batch" {
		t.Fatalf("AI calls = %s", got)
	}
	if got := *ai.parsed.Load(); got != "Ada Okafor\nSenior Accountant, CPA" {
		t.Fatalf("parsed text = %q", got)
	}

	var headline, availability, timezone, from, model string
	var years int
	var certs, software []string
	var stored []byte
	err := pool.QueryRow(ctx, `
		SELECT headline, years_experience, certifications, software, availability, timezone, available_from::text, profile, embedding_model
		FROM candidate_profiles WHERE candidate_id = $1 AND embedding IS NOT NULL AND embedded_at IS NOT NULL`, cand).
		Scan(&headline, &years, &certs, &software, &availability, &timezone, &from, &stored, &model)
	if err != nil {
		t.Fatalf("profile with an embedding not found: %v", err)
	}
	if headline != "Senior Accountant, CPA" || years != 8 || availability != "two_weeks" || timezone != "America/Chicago" || from != "2026-11-02" || model != "fake" {
		t.Fatalf("columns: %q %d %q %q %q %q", headline, years, availability, timezone, from, model)
	}
	if strings.Join(certs, ",") != "cpa_us" || strings.Join(software, ",") != "netsuite,quickbooks" {
		t.Fatalf("hard-filter columns: %v %v", certs, software)
	}
	// The JSONB document is the parser's whole extraction, not just the columns.
	var doc map[string]any
	if err := json.Unmarshal(stored, &doc); err != nil || doc["positions"] == nil || doc["skills"] == nil || doc["other_certifications"] == nil {
		t.Fatalf("stored profile: %s (%v)", stored, err)
	}

	// Running it again (a worker that died after the write) replaces the profile in place.
	if err := handler(ctx, job(KindParseResume, "candidate_id", cand)); err != nil {
		t.Fatal(err)
	}
	if n := profiles(t, pool, cand); n != 1 {
		t.Fatalf("want 1 profile after a re-run, got %d", n)
	}
}

// Values the model gets wrong in the soft fields are left out; the profile
// is still written.
func TestParseResumeDropsValuesTheColumnsCannotHold(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	ai := &intakeAI{profile: `{"headline":"  ","years_experience":140,"availability":"soon","timezone":"Mars/Olympus","certifications":[],"software":[]}`}
	cand := newCandidate(t, pool, "resume")
	if err := intake(t, pool, ai)(ctx, job(KindParseResume, "candidate_id", cand)); err != nil {
		t.Fatal(err)
	}
	var headline, timezone *string
	var years *int
	var availability string
	if err := pool.QueryRow(ctx, `SELECT headline, years_experience, availability, timezone FROM candidate_profiles WHERE candidate_id = $1`, cand).
		Scan(&headline, &years, &availability, &timezone); err != nil {
		t.Fatal(err)
	}
	if headline != nil || years != nil || timezone != nil || availability != "unknown" {
		t.Fatalf("columns: %v %v %v %q", headline, years, timezone, availability)
	}
}

func TestParseResumeFailuresAreClassified(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	cand := newCandidate(t, pool, "resume")
	run := func(ai *intakeAI, candidate string) error {
		return intake(t, pool, ai)(ctx, job(KindParseResume, "candidate_id", candidate))
	}

	// The model's output did not validate, or the provider is down: retry.
	if err := run(&intakeAI{parseStatus: 502}, cand); err == nil || errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("502: err = %v, want a retryable error", err)
	}
	// The service will not take this text at all: do not retry.
	if err := run(&intakeAI{parseStatus: 422}, cand); !errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("422: err = %v, want permanent", err)
	}
	// An id the API's taxonomy does not have would never match a hard filter.
	err := run(&intakeAI{profile: `{"certifications":["cpa_us","cpa_mars"],"software":["abacus"]}`}, cand)
	if !errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "cpa_mars") || !strings.Contains(err.Error(), "abacus") {
		t.Fatalf("unknown ids: err = %v, want permanent naming them", err)
	}
	if n := profiles(t, pool, cand); n != 0 {
		t.Fatalf("failed parses wrote %d profiles", n)
	}

	// Nothing to parse; a candidate deleted since the upload; a bad payload.
	ai := &intakeAI{}
	if err := run(ai, newCandidate(t, pool, "  ")); !errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("blank resume: err = %v, want permanent", err)
	}
	if err := run(ai, "22222222-0000-0000-0000-000000000002"); err != nil {
		t.Fatalf("missing candidate: %v", err)
	}
	if err := run(ai, "not-a-uuid"); !errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("bad id: err = %v, want permanent", err)
	}
	if len(ai.calls) != 0 {
		t.Fatalf("AI service called for nothing to parse: %v", ai.calls)
	}
}

// A resume uploaded while the old one is being parsed makes that parse
// stale: it is not written, and the retry parses the new text.
func TestParseResumeDoesNotWriteAStaleParse(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	cand := newCandidate(t, pool, "old resume")
	ai := &intakeAI{during: func() {
		if _, err := pool.Exec(ctx, `UPDATE candidates SET resume_text = 'new resume' WHERE id = $1`, cand); err != nil {
			t.Error(err)
		}
	}}
	err := intake(t, pool, ai)(ctx, job(KindParseResume, "candidate_id", cand))
	if err == nil || errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "resume replaced") {
		t.Fatalf("upload during parse: err = %v, want a retryable 'resume replaced' error", err)
	}
	if n := profiles(t, pool, cand); n != 0 {
		t.Fatal("a profile parsed from the old resume was written")
	}
	ai = &intakeAI{}
	if err := intake(t, pool, ai)(ctx, job(KindParseResume, "candidate_id", cand)); err != nil {
		t.Fatal(err)
	}
	if got := *ai.parsed.Load(); got != "new resume" || profiles(t, pool, cand) != 1 {
		t.Fatalf("retry parsed %q", got)
	}
}

// When only the embedding fails, the parse is not thrown away: the profile
// stays, the job succeeds, and an embed_profile job takes over.
func TestParseResumeHandsAFailedEmbeddingToItsOwnJob(t *testing.T) {
	old := logf
	logf = func(context.Context, string, ...any) {}
	t.Cleanup(func() { logf = old })

	pool := dbtest.Pool(t)
	ctx := context.Background()
	cand := newCandidate(t, pool, "resume")
	if err := intake(t, pool, &intakeAI{embedStatus: 503})(ctx, job(KindParseResume, "candidate_id", cand)); err != nil {
		t.Fatalf("embedding failure should not fail the parse job: %v", err)
	}
	if _, ok := embedded(t, pool, "candidate_profiles", "candidate_id", cand); ok {
		t.Fatal("no vector expected")
	}
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind = $1 AND status = 'queued' AND payload->>'candidate_id' = $2`, KindEmbedProfile, cand).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("want 1 queued embed_profile job, got %d", queued)
	}
}
