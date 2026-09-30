package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/dbtest"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeAI answers /embed-batch with a deterministic vector, or with a status
// code. last is what the most recent request asked to embed: the text, or
// the structured document as JSON.
type fakeAI struct {
	calls  atomic.Int32
	status int
	dim    int
	last   atomic.Pointer[string]
	during func() // runs while the "provider" is embedding, before it answers
	// rejectStructured answers 422 to a profile or requirements input, as
	// the real service does for a document that is not valid for its model.
	rejectStructured bool
}

func (f *fakeAI) server(t *testing.T) *aiclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var req struct {
			Inputs []aiclient.EmbedItem `json:"inputs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/embed-batch" || len(req.Inputs) != 1 {
			t.Errorf("unexpected request %s with %d inputs", r.URL.Path, len(req.Inputs))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		in := req.Inputs[0]
		asked := in.Text + string(in.Profile) + string(in.Requirements)
		f.last.Store(&asked)
		if f.during != nil {
			f.during()
		}
		status := f.status
		if f.rejectStructured && in.Text == "" {
			status = http.StatusUnprocessableEntity
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"nope"}`))
			return
		}
		dim := f.dim
		if dim == 0 {
			dim = EmbeddingDim
		}
		vec := make([]float32, dim)
		vec[0] = 1
		_ = json.NewEncoder(w).Encode(aiclient.EmbedBatchResponse{Embeddings: [][]float32{vec}, Texts: []string{asked}, Dim: dim, Provider: "fake"})
	}))
	t.Cleanup(srv.Close)
	return aiclient.New(srv.URL)
}

// sent decodes the structured document the fake was last asked to embed.
func (f *fakeAI) sent(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(*f.last.Load()), &doc); err != nil {
		t.Fatalf("last input is not a JSON document: %q", *f.last.Load())
	}
	return doc
}

func job(kind, field, id string) jobs.Job {
	return jobs.Job{ID: 1, Kind: kind, Payload: json.RawMessage(fmt.Sprintf(`{%q:%q}`, field, id))}
}

func newRole(t *testing.T, pool *pgxpool.Pool, description string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO roles (title, description) VALUES ('Controller', $1) RETURNING id::text`, description).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func embedded(t *testing.T, pool *pgxpool.Pool, table, key, id string) (model *string, hasVector bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT embedding_model, embedding IS NOT NULL FROM `+table+` WHERE `+key+` = $1`, id).Scan(&model, &hasVector); err != nil {
		t.Fatal(err)
	}
	return model, hasVector
}

func TestEmbedRoleWritesVector(t *testing.T) {
	pool := dbtest.Pool(t)
	ai := &fakeAI{}
	reg := Registry(pool, ai.server(t))
	id := newRole(t, pool, "Owns the monthly close.")

	if err := reg[KindEmbedRole](context.Background(), job(KindEmbedRole, "role_id", id)); err != nil {
		t.Fatal(err)
	}
	model, ok := embedded(t, pool, "roles", "id", id)
	if !ok || model == nil || *model != "fake" {
		t.Fatalf("embedding not written: model=%v vector=%v", model, ok)
	}
	if got := *ai.last.Load(); got != "Owns the monthly close." {
		t.Fatalf("embedded text = %q", got)
	}
	// Idempotent: the same job again re-embeds the same text.
	if err := reg[KindEmbedRole](context.Background(), job(KindEmbedRole, "role_id", id)); err != nil {
		t.Fatal(err)
	}
}

func TestEmbedRoleSkipsBlankAndMissingRows(t *testing.T) {
	pool := dbtest.Pool(t)
	ai := &fakeAI{}
	reg := Registry(pool, ai.server(t))

	blank := newRole(t, pool, "   ")
	if err := reg[KindEmbedRole](context.Background(), job(KindEmbedRole, "role_id", blank)); err != nil {
		t.Fatal(err)
	}
	if _, ok := embedded(t, pool, "roles", "id", blank); ok {
		t.Fatal("blank description should leave the embedding NULL")
	}
	// Deleted since it was queued: done, not an error.
	if err := reg[KindEmbedRole](context.Background(), job(KindEmbedRole, "role_id", "22222222-0000-0000-0000-000000000002")); err != nil {
		t.Fatalf("missing row: %v", err)
	}
	if ai.calls.Load() != 0 {
		t.Fatalf("AI service called %d times for nothing to embed", ai.calls.Load())
	}
	// Garbage payloads are permanent failures.
	for _, j := range []jobs.Job{
		{Kind: KindEmbedRole, Payload: json.RawMessage(`{}`)},
		{Kind: KindEmbedRole, Payload: json.RawMessage(`{"role_id":"not-a-uuid"}`)},
		{Kind: KindEmbedRole, Payload: json.RawMessage(`[1]`)},
	} {
		err := reg[KindEmbedRole](context.Background(), j)
		if !errors.Is(err, jobs.ErrPermanent) {
			t.Errorf("payload %s: err = %v, want permanent", j.Payload, err)
		}
	}
}

func TestEmbedErrorsAreClassified(t *testing.T) {
	pool := dbtest.Pool(t)
	id := newRole(t, pool, "text")
	ctx := context.Background()

	// 4xx from the AI service: our input is wrong, do not retry.
	bad := &fakeAI{status: 422}
	if err := Registry(pool, bad.server(t))[KindEmbedRole](ctx, job(KindEmbedRole, "role_id", id)); !errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("4xx: err = %v, want permanent", err)
	}
	// 5xx: transient, retry.
	down := &fakeAI{status: 503}
	if err := Registry(pool, down.server(t))[KindEmbedRole](ctx, job(KindEmbedRole, "role_id", id)); err == nil || errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("503: err = %v, want a retryable error", err)
	}
	// Wrong width can never be stored.
	narrow := &fakeAI{dim: 8}
	if err := Registry(pool, narrow.server(t))[KindEmbedRole](ctx, job(KindEmbedRole, "role_id", id)); !errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "8-dimension") {
		t.Fatalf("wrong dim: err = %v", err)
	}
}

// A profile whose JSON is not a CandidateProfile is refused as a structured
// input and embedded as plain text instead.
func TestEmbedProfileFallsBackToTextForFreeFormJSON(t *testing.T) {
	pool := dbtest.Pool(t)
	ai := &fakeAI{rejectStructured: true}
	reg := Registry(pool, ai.server(t))
	ctx := context.Background()

	var cand string
	if err := pool.QueryRow(ctx, `INSERT INTO candidates (full_name) VALUES ('Dana Ito') RETURNING id::text`).Scan(&cand); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO candidate_profiles (candidate_id, headline, profile, certifications, software)
		VALUES ($1, 'Senior accountant', '{"summary":"12 years in close"}', '{cpa}', '{netsuite,quickbooks}')`, cand); err != nil {
		t.Fatal(err)
	}
	if err := reg[KindEmbedProfile](ctx, job(KindEmbedProfile, "candidate_id", cand)); err != nil {
		t.Fatal(err)
	}
	if _, ok := embedded(t, pool, "candidate_profiles", "candidate_id", cand); !ok {
		t.Fatal("profile embedding not written")
	}
	want := "Senior accountant\n{\"summary\": \"12 years in close\"}\ncertifications: cpa\nsoftware: netsuite, quickbooks"
	if got := *ai.last.Load(); got != want {
		t.Fatalf("embedded text:\n%q\nwant:\n%q", got, want)
	}
	if ai.calls.Load() != 2 {
		t.Fatalf("want the structured attempt then the text fallback, got %d calls", ai.calls.Load())
	}
}

// A CandidateProfile is sent as a document, for the AI service to render to
// the canonical text, with the columns laid over the stored JSON.
func TestEmbedProfileSendsTheStructuredProfile(t *testing.T) {
	pool := dbtest.Pool(t)
	ai := &fakeAI{}
	reg := Registry(pool, ai.server(t))
	ctx := context.Background()

	var cand string
	if err := pool.QueryRow(ctx, `INSERT INTO candidates (full_name) VALUES ('Ana Ruiz') RETURNING id::text`).Scan(&cand); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO candidate_profiles (candidate_id, headline, profile, certifications, software)
		VALUES ($1, 'Payroll Manager', '{"headline":"stale","skills":["multi-state payroll"],"software":["adp"],"industries":["healthcare"]}', '{cpp}', '{}')`, cand); err != nil {
		t.Fatal(err)
	}
	if err := reg[KindEmbedProfile](ctx, job(KindEmbedProfile, "candidate_id", cand)); err != nil {
		t.Fatal(err)
	}
	if _, ok := embedded(t, pool, "candidate_profiles", "candidate_id", cand); !ok {
		t.Fatal("profile embedding not written")
	}
	got, _ := json.Marshal(ai.sent(t))
	// The headline and certifications columns win; an empty column leaves the JSON's list alone.
	want := `{"certifications":["cpp"],"headline":"Payroll Manager","industries":["healthcare"],"skills":["multi-state payroll"],"software":["adp"]}`
	if string(got) != want {
		t.Fatalf("sent profile:\n%s\nwant:\n%s", got, want)
	}
	if ai.calls.Load() != 1 {
		t.Fatalf("want 1 call, got %d", ai.calls.Load())
	}
}

// A role with structured requirements is embedded from them, not from the
// raw JD, so its vector is comparable with a profile's.
func TestEmbedRoleSendsTheStructuredRequirements(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	insert := func(requirements, must string) string {
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO roles (title, description, requirements, must_haves, required_software)
			VALUES ('Senior Accountant', 'We are hiring.', $1, $2, '{netsuite}') RETURNING id::text`, requirements, must).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	ai := &fakeAI{}
	id := insert(`{"title":"stale","industries":["healthcare"],"must_haves":["stale"]}`, `["Runs the close"]`)
	if err := Registry(pool, ai.server(t))[KindEmbedRole](ctx, job(KindEmbedRole, "role_id", id)); err != nil {
		t.Fatal(err)
	}
	if _, ok := embedded(t, pool, "roles", "id", id); !ok {
		t.Fatal("role embedding not written")
	}
	got, _ := json.Marshal(ai.sent(t))
	want := `{"industries":["healthcare"],"must_haves":["Runs the close"],"required_software":["netsuite"],"title":"Senior Accountant"}`
	if string(got) != want {
		t.Fatalf("sent requirements:\n%s\nwant:\n%s", got, want)
	}

	// Requirements the AI service will not take: the description is embedded instead.
	ai = &fakeAI{rejectStructured: true}
	id = insert(`{"not_a_requirements_field":1}`, `[]`)
	if err := Registry(pool, ai.server(t))[KindEmbedRole](ctx, job(KindEmbedRole, "role_id", id)); err != nil {
		t.Fatal(err)
	}
	if got := *ai.last.Load(); got != "We are hiring." || ai.calls.Load() != 2 {
		t.Fatalf("fallback embedded %q in %d calls", got, ai.calls.Load())
	}

	// ...and with no description to fall back on, the rejection is permanent.
	if _, err := pool.Exec(ctx, `UPDATE roles SET description = '' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := Registry(pool, ai.server(t))[KindEmbedRole](ctx, job(KindEmbedRole, "role_id", id)); !errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("rejected requirements with no description: err = %v, want permanent", err)
	}

	// A requirement edited mid-embed makes the vector stale, like a description edit.
	edited := insert(`{}`, `["Runs the close"]`)
	ai = &fakeAI{during: func() {
		if _, err := pool.Exec(ctx, `UPDATE roles SET must_haves = '["Runs the close and the audit"]' WHERE id = $1`, edited); err != nil {
			t.Error(err)
		}
	}}
	err := Registry(pool, ai.server(t))[KindEmbedRole](ctx, job(KindEmbedRole, "role_id", edited))
	if err == nil || errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "text changed") {
		t.Fatalf("must-have edit during embedding: err = %v, want a retryable 'text changed' error", err)
	}
	if _, ok := embedded(t, pool, "roles", "id", edited); ok {
		t.Fatal("a stale vector was written over the new requirements")
	}
}

func TestVectorLiteral(t *testing.T) {
	if got := vectorLiteral([]float32{1, -0.5, 0.25}); got != "[1,-0.5,0.25]" {
		t.Fatalf("got %s", got)
	}
}

// The vector is written only if the text it came from is still the row's
// text. An unrelated edit during the provider call must not get in the way;
// a text edit must not be overwritten with the stale vector.
func TestEmbedWriteGuardsOnTheEmbeddedText(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	// Status changes mid-embed: irrelevant to the vector, so it lands.
	statusEdit := newRole(t, pool, "Owns the close.")
	ai := &fakeAI{during: func() {
		if _, err := pool.Exec(ctx, `UPDATE roles SET status = 'filled' WHERE id = $1`, statusEdit); err != nil {
			t.Error(err)
		}
	}}
	if err := Registry(pool, ai.server(t))[KindEmbedRole](ctx, job(KindEmbedRole, "role_id", statusEdit)); err != nil {
		t.Fatalf("unrelated edit during embedding should not fail the job: %v", err)
	}
	if _, ok := embedded(t, pool, "roles", "id", statusEdit); !ok {
		t.Fatal("vector should have been written despite the status edit")
	}

	// Description changes mid-embed: the vector is stale, so it is not
	// written and the job asks to be retried (the new text gets embedded).
	textEdit := newRole(t, pool, "Owns the close.")
	ai = &fakeAI{during: func() {
		if _, err := pool.Exec(ctx, `UPDATE roles SET description = 'Owns the close and the audit.' WHERE id = $1`, textEdit); err != nil {
			t.Error(err)
		}
	}}
	err := Registry(pool, ai.server(t))[KindEmbedRole](ctx, job(KindEmbedRole, "role_id", textEdit))
	if err == nil || errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "text changed") {
		t.Fatalf("text edit during embedding: err = %v, want a retryable 'text changed' error", err)
	}
	if _, ok := embedded(t, pool, "roles", "id", textEdit); ok {
		t.Fatal("a stale vector was written over the new text")
	}
	// The retry embeds the current text.
	ai = &fakeAI{}
	if err := Registry(pool, ai.server(t))[KindEmbedRole](ctx, job(KindEmbedRole, "role_id", textEdit)); err != nil {
		t.Fatal(err)
	}
	if got := *ai.last.Load(); got != "Owns the close and the audit." {
		t.Fatalf("retry embedded %q", got)
	}

	// Profiles: the guard covers the same fields the store clears on.
	var cand string
	if err := pool.QueryRow(ctx, `INSERT INTO candidates (full_name) VALUES ('Eli Park') RETURNING id::text`).Scan(&cand); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO candidate_profiles (candidate_id, headline, certifications) VALUES ($1, 'Controller', '{cpa}')`, cand); err != nil {
		t.Fatal(err)
	}
	ai = &fakeAI{during: func() {
		if _, err := pool.Exec(ctx, `UPDATE candidate_profiles SET availability = 'immediate' WHERE candidate_id = $1`, cand); err != nil {
			t.Error(err)
		}
	}}
	if err := Registry(pool, ai.server(t))[KindEmbedProfile](ctx, job(KindEmbedProfile, "candidate_id", cand)); err != nil {
		t.Fatalf("availability edit during embedding should not fail the job: %v", err)
	}
	if _, ok := embedded(t, pool, "candidate_profiles", "candidate_id", cand); !ok {
		t.Fatal("profile vector should have been written despite the availability edit")
	}
}
