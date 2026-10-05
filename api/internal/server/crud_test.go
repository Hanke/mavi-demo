package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/colehanke/mavi-demo/api/internal/dbtest"
	"github.com/colehanke/mavi-demo/api/internal/jobs"
	"github.com/colehanke/mavi-demo/api/internal/store"
	"github.com/colehanke/mavi-demo/api/internal/tasks"
	"github.com/colehanke/mavi-demo/api/internal/taxonomy"
	"github.com/jackc/pgx/v5/pgxpool"
)

// api drives a server backed by a throwaway database.
type api struct {
	t    *testing.T
	h    http.Handler
	pool *pgxpool.Pool
}

func newAPI(t *testing.T) *api { return newAPIWith(t, stub{}) }

// newAPIWith is newAPI with a particular stand-in for the AI service.
func newAPIWith(t *testing.T, ai stub) *api {
	t.Helper()
	return newAPIOn(t, dbtest.Pool(t), ai, 0)
}

// newAPIOn is a server over an existing database that retrieves up to
// retrieve candidates per filter run (0 for the default).
func newAPIOn(t *testing.T, pool *pgxpool.Pool, ai stub, retrieve int) *api {
	t.Helper()
	tax, err := taxonomy.Load(dbtest.TaxonomyPath())
	if err != nil {
		t.Fatal(err)
	}
	embedRole := func(ctx context.Context, roleID string) error { return tasks.EmbedRole(ctx, pool, ai, roleID) }
	h := New(Config{DB: pool, AI: ai, Store: store.New(pool), Taxonomy: tax, Jobs: jobs.NewQueue(pool), JobKinds: tasks.Registry(pool, nil, nil, tasks.MatchConfig{}).Kinds(), EmbedRole: embedRole, RetrievalSize: retrieve, CORSOrigin: "*"})
	return &api{t: t, h: h, pool: pool}
}

type resp struct {
	Code int
	Body map[string]any
	List []map[string]any
	Raw  string
}

func (r resp) str(k string) string {
	v, _ := r.Body[k].(string)
	return v
}

func (r resp) field(k string) string {
	f, _ := r.Body["fields"].(map[string]any)
	v, _ := f[k].(string)
	return v
}

// do sends a request as role/actor with an optional JSON body.
func (a *api) do(method, path, role, actor string, body any) resp {
	a.t.Helper()
	var reader *strings.Reader
	if body != nil {
		var buf []byte
		if s, ok := body.(string); ok {
			buf = []byte(s)
		} else {
			buf, _ = json.Marshal(body)
		}
		reader = strings.NewReader(string(buf))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	if role != "" {
		req.Header.Set("X-Role", role)
	}
	if actor != "" {
		req.Header.Set("X-Actor", actor)
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	out := resp{Code: rec.Code, Raw: rec.Body.String()}
	trimmed := strings.TrimSpace(out.Raw)
	switch {
	case strings.HasPrefix(trimmed, "{"):
		_ = json.Unmarshal([]byte(trimmed), &out.Body)
	case strings.HasPrefix(trimmed, "["):
		_ = json.Unmarshal([]byte(trimmed), &out.List)
	}
	return out
}

// want fails the test unless the response has the expected status.
func (a *api) want(r resp, code int, what string) resp {
	a.t.Helper()
	if r.Code != code {
		a.t.Fatalf("%s: status = %d, want %d; body: %s", what, r.Code, code, r.Raw)
	}
	return r
}

func (a *api) count(sql string, args ...any) int {
	a.t.Helper()
	var n int
	if err := a.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	return n
}

func (a *api) exec(sql string, args ...any) {
	a.t.Helper()
	if _, err := a.pool.Exec(context.Background(), sql, args...); err != nil {
		a.t.Fatal(err)
	}
}

func (a *api) candidate(name, email string) string {
	a.t.Helper()
	r := a.want(a.do("POST", "/candidates", "ops", "", map[string]any{"full_name": name, "email": email}), 201, "create candidate")
	return r.str("id")
}

func (a *api) role(title string) string {
	a.t.Helper()
	r := a.want(a.do("POST", "/roles", "employer", "", map[string]any{"title": title, "company": "Acme"}), 201, "create role")
	return r.str("id")
}

// match proposes a match, first giving the candidate the availability
// without which they cannot be matched.
func (a *api) match(roleID, candID string, score float64) string {
	a.t.Helper()
	a.do("PUT", "/candidates/"+candID+"/availability", "ops", "", workHours("America/Chicago", "09:00", "17:00", 40, "2020-01-01"))
	r := a.want(a.do("POST", "/matches", "ops", "", map[string]any{"role_id": roleID, "candidate_id": candID, "score": score}), 201, "create match")
	return r.str("id")
}

func TestCandidateCRUD(t *testing.T) {
	a := newAPI(t)

	r := a.want(a.do("POST", "/candidates", "ops", "", map[string]any{
		"full_name": "  Ada Okafor ", "email": "ada@example.com", "location": "Chicago, IL", "resume_text": "CPA, NetSuite",
	}), 201, "create")
	id := r.str("id")
	if r.str("full_name") != "Ada Okafor" || r.str("status") != "active" {
		t.Fatalf("unexpected create body: %s", r.Raw)
	}

	// Validation: missing name, bad email, bad status, unknown field, bad JSON.
	a.want(a.do("POST", "/candidates", "ops", "", map[string]any{"email": "x@y"}), 422, "missing name")
	r = a.want(a.do("POST", "/candidates", "ops", "", map[string]any{"full_name": "B", "email": "nope", "status": "gone"}), 422, "bad fields")
	if r.field("email") == "" || r.field("status") == "" {
		t.Fatalf("expected field errors, got %s", r.Raw)
	}
	a.want(a.do("POST", "/candidates", "ops", "", map[string]any{"full_name": "B", "nickname": "b"}), 400, "unknown field")
	a.want(a.do("POST", "/candidates", "ops", "", "{not json"), 400, "bad json")
	// Unique email, case-insensitively.
	a.want(a.do("POST", "/candidates", "ops", "", map[string]any{"full_name": "Dup", "email": "ADA@example.com"}), 409, "duplicate email")

	// Read and list.
	r = a.want(a.do("GET", "/candidates/"+id, "ops", "", nil), 200, "get")
	if r.str("email") != "ada@example.com" {
		t.Fatalf("get: %s", r.Raw)
	}
	a.want(a.do("GET", "/candidates/not-a-uuid", "ops", "", nil), 404, "get bad id")
	r = a.want(a.do("GET", "/candidates?status=active", "ops", "", nil), 200, "list")
	if len(r.List) != 1 {
		t.Fatalf("list: want 1, got %s", r.Raw)
	}
	a.want(a.do("GET", "/candidates?status=bogus", "ops", "", nil), 422, "list bad status")

	// Update keeps omitted fields, clears explicit nulls.
	r = a.want(a.do("PUT", "/candidates/"+id, "ops", "", map[string]any{"status": "archived", "location": nil}), 200, "update")
	if r.str("status") != "archived" || r.Body["location"] != nil || r.str("full_name") != "Ada Okafor" {
		t.Fatalf("update: %s", r.Raw)
	}
	a.want(a.do("PUT", "/candidates/"+id, "ops", "", map[string]any{"full_name": ""}), 422, "update blank name")
	a.want(a.do("PUT", "/candidates/"+id, "ops", "", map[string]any{"full_name": nil}), 422, "update null name")
	a.want(a.do("PUT", "/candidates/"+id, "ops", "", map[string]any{"status": nil}), 422, "update null status")
	r = a.want(a.do("PUT", "/candidates/"+id, "ops", "", map[string]any{"resume_text": nil}), 200, "update null resume")
	if r.str("resume_text") != "" {
		t.Fatalf("null resume_text should clear it: %s", r.Raw)
	}
	r = a.want(a.do("GET", "/candidates?status=active", "ops", "", nil), 200, "list after archive")
	if len(r.List) != 0 {
		t.Fatalf("archived candidate still listed as active: %s", r.Raw)
	}

	// Delete.
	a.want(a.do("DELETE", "/candidates/"+id, "ops", "", nil), 204, "delete")
	a.want(a.do("DELETE", "/candidates/"+id, "ops", "", nil), 404, "delete again")
	a.want(a.do("GET", "/candidates/"+id, "ops", "", nil), 404, "get after delete")
}

func TestTalentIsScopedToOwnCandidate(t *testing.T) {
	a := newAPI(t)

	// Talent creates their own record and gets an id back to use as actor.
	r := a.want(a.do("POST", "/candidates", "talent", "", map[string]any{"full_name": "Ben Larsen"}), 201, "talent create")
	mine := r.str("id")
	if r.str("source") != "self" {
		t.Fatalf("talent-created candidate should be marked self-sourced: %s", r.Raw)
	}
	other := a.candidate("Chloe Martin", "chloe@example.com")

	a.want(a.do("GET", "/candidates/"+mine, "talent", "", nil), 403, "talent without actor")
	a.want(a.do("GET", "/candidates/"+mine, "talent", "ben", nil), 403, "talent with non-uuid actor")
	a.want(a.do("GET", "/candidates/"+mine, "talent", mine, nil), 200, "talent own")
	a.want(a.do("GET", "/candidates/"+other, "talent", mine, nil), 404, "talent other")
	a.want(a.do("PUT", "/candidates/"+other, "talent", mine, map[string]any{"full_name": "Hijack"}), 404, "talent update other")
	r = a.want(a.do("PUT", "/candidates/"+mine, "talent", mine, map[string]any{"location": "Denver, CO"}), 200, "talent update own")
	if r.str("location") != "Denver, CO" {
		t.Fatalf("update: %s", r.Raw)
	}
	a.want(a.do("GET", "/candidates/"+other, "employer", "", nil), 403, "employer reads candidate")
	a.want(a.do("DELETE", "/candidates/"+mine, "talent", mine, nil), 403, "talent delete")

	// Whether a candidate is in the pool, and where they came from, is ops'
	// to say: a candidate ops archived cannot put themselves back.
	a.want(a.do("PUT", "/candidates/"+mine, "ops", "", map[string]any{"status": "archived"}), 200, "ops archives")
	r = a.want(a.do("PUT", "/candidates/"+mine, "talent", mine, map[string]any{"status": "active", "source": "agency"}), 422, "talent un-archives")
	if r.field("status") == "" || r.field("source") == "" {
		t.Fatalf("want field errors on status and source: %s", r.Raw)
	}
	// Sending the record back as it was read is not a change.
	r = a.want(a.do("PUT", "/candidates/"+mine, "talent", mine, map[string]any{"status": "archived", "source": "self", "phone": "555-0100"}), 200, "talent sends status back unchanged")
	if r.str("status") != "archived" || r.str("phone") != "555-0100" {
		t.Fatalf("update: %s", r.Raw)
	}
	a.want(a.do("POST", "/candidates", "talent", "", map[string]any{"full_name": "Dee", "status": "archived"}), 422, "talent signs up archived")
	a.want(a.do("POST", "/candidates", "talent", "", map[string]any{"full_name": "Dee", "source": "referral"}), 422, "talent names a source")
	a.want(a.do("POST", "/candidates", "talent", "", map[string]any{"full_name": "Dee", "status": "active", "source": "self"}), 201, "talent sends the defaults")
}

// Text Postgres cannot store is refused at the door, not a 500 at the write.
func TestUnstorableInputIsRefused(t *testing.T) {
	a := newAPI(t)
	id := a.candidate("Ada Okafor", "ada@example.com")
	a.want(a.do("POST", "/candidates", "ops", "", `{"full_name":"a\u0000b"}`), 400, "NUL escape in a name")
	a.want(a.do("POST", "/candidates", "ops", "", "{\"full_name\":\"a\x00b\"}"), 400, "raw NUL in a name")
	a.want(a.do("POST", "/jobs", "ops", "", `{"kind":"embed_role","payload":{"role_id":"\u0000"}}`), 400, "NUL in a job payload")
	// The six characters of the escape, spelled out, are ordinary text.
	r := a.want(a.do("POST", "/candidates", "ops", "", `{"full_name":"a\\u0000b"}`), 201, "an escaped backslash")
	if r.str("full_name") != `a\u0000b` {
		t.Fatalf("stored %q", r.str("full_name"))
	}
	for _, clock := range []string{"+9:00", "-0:30", "9:000", " 9:00", "24:00"} {
		r := a.want(a.do("PUT", "/candidates/"+id+"/availability", "ops", "", workHours("America/Chicago", clock, "17:00", 40, "2026-01-01")), 422, "work_start "+clock)
		if r.field("work_start") == "" {
			t.Fatalf("work_start %q: %s", clock, r.Raw)
		}
	}
}

func TestProfileUpsertResolvesTaxonomy(t *testing.T) {
	a := newAPI(t)
	id := a.candidate("Ada Okafor", "ada@example.com")

	a.want(a.do("GET", "/candidates/"+id+"/profile", "ops", "", nil), 404, "no profile yet")

	r := a.want(a.do("PUT", "/candidates/"+id+"/profile", "talent", id, map[string]any{
		"headline":       "Senior Accountant",
		"certifications": []string{"CPA", "Certified Public Accountant"},
		"software":       []string{"QuickBooks Online", "NetSuite"},
		"availability":   "two_weeks",
		"available_from": "2026-10-14",
		"timezone":       "America/Chicago",
		"profile":        map[string]any{"skills": []string{"month-end close"}},
	}), 201, "create profile")
	if got := fmt.Sprint(r.Body["certifications"]); got != "[cpa]" {
		t.Fatalf("certifications = %s", got)
	}
	if got := fmt.Sprint(r.Body["software"]); got != "[quickbooks netsuite]" {
		t.Fatalf("software = %s", got)
	}
	if r.str("available_from") != "2026-10-14" {
		t.Fatalf("available_from = %s", r.Raw)
	}

	// Replace: second PUT is a 200 and fully replaces the lists.
	r = a.want(a.do("PUT", "/candidates/"+id+"/profile", "ops", "", map[string]any{"software": []string{"excel"}}), 200, "replace profile")
	if got := fmt.Sprint(r.Body["software"]); got != "[excel]" || fmt.Sprint(r.Body["certifications"]) != "[]" {
		t.Fatalf("replace: %s", r.Raw)
	}
	if r.str("availability") != "unknown" {
		t.Fatalf("availability should default to unknown: %s", r.Raw)
	}

	// Unknown taxonomy value, bad timezone, bad availability, non-object profile.
	r = a.want(a.do("PUT", "/candidates/"+id+"/profile", "ops", "", map[string]any{
		"software": []string{"netsuite", "MS Paint"}, "timezone": "Mars/Olympus", "availability": "soon", "profile": []int{1},
	}), 422, "bad profile")
	for _, f := range []string{"software", "timezone", "availability", "profile"} {
		if r.field(f) == "" {
			t.Errorf("expected a field error for %s: %s", f, r.Raw)
		}
	}
	if !strings.Contains(r.field("software"), "MS Paint") {
		t.Errorf("software error should name the unknown value: %s", r.field("software"))
	}

	// Nothing non-canonical ever lands in the hard-filter columns.
	if n := a.count(`SELECT count(*) FROM candidate_profiles, unnest(software) v WHERE v <> lower(v) OR v LIKE '% %'`); n != 0 {
		t.Fatalf("non-canonical software values stored: %d", n)
	}

	a.want(a.do("PUT", "/candidates/00000000-0000-0000-0000-000000000000/profile", "ops", "", map[string]any{}), 404, "profile for missing candidate")

	// Re-saving identical content keeps the embedding; a real change clears it.
	a.want(a.do("PUT", "/candidates/"+id+"/profile", "ops", "", map[string]any{"headline": "Controller", "software": []string{"excel"}}), 200, "reset profile")
	a.exec(`UPDATE candidate_profiles SET embedding_model = 'stub', embedded_at = now() WHERE candidate_id = $1`, id)
	r = a.want(a.do("PUT", "/candidates/"+id+"/profile", "ops", "", map[string]any{"headline": "Controller", "software": []string{"Excel"}}), 200, "resave unchanged")
	if r.str("embedding_model") != "stub" {
		t.Fatalf("unchanged profile should keep its embedding: %s", r.Raw)
	}
	r = a.want(a.do("PUT", "/candidates/"+id+"/profile", "ops", "", map[string]any{"headline": "VP Finance", "software": []string{"Excel"}}), 200, "resave changed")
	if r.Body["embedding_model"] != nil {
		t.Fatalf("changed profile should clear its embedding: %s", r.Raw)
	}
	a.want(a.do("DELETE", "/candidates/"+id+"/profile", "talent", id, nil), 403, "talent delete profile")
	a.want(a.do("DELETE", "/candidates/"+id+"/profile", "ops", "", nil), 204, "delete profile")
	a.want(a.do("GET", "/candidates/"+id+"/profile", "ops", "", nil), 404, "profile gone")
}

func TestRoleCRUD(t *testing.T) {
	a := newAPI(t)

	r := a.want(a.do("POST", "/roles", "employer", "", map[string]any{
		"title": "Senior Accountant", "company": "Northwind", "description": "Own month-end close.",
		"must_haves": []string{"cpa", "netsuite"}, "required_certifications": []string{"CPA"},
		"required_software": []string{"NetSuite", "QBO"}, "min_years_experience": 5,
		"timezone": "America/Chicago", "starts_on": "2026-11-01",
	}), 201, "create role")
	id := r.str("id")
	if got := fmt.Sprint(r.Body["required_software"]); got != "[netsuite quickbooks]" {
		t.Fatalf("required_software = %s", got)
	}
	if r.str("status") != "open" || r.str("starts_on") != "2026-11-01" || r.Body["min_years_experience"] != float64(5) {
		t.Fatalf("create: %s", r.Raw)
	}
	a.want(a.do("POST", "/roles", "employer", "", map[string]any{"title": "x", "min_years_experience": 99}), 422, "bad years")
	if none := a.want(a.do("PUT", "/roles/"+id, "employer", "", map[string]any{"min_years_experience": 0}), 200, "no minimum"); none.Body["min_years_experience"] != nil {
		t.Fatalf("a minimum of 0 should be stored as null: %s", none.Raw)
	}

	a.want(a.do("POST", "/roles", "employer", "", map[string]any{"company": "x"}), 422, "missing title")
	r = a.want(a.do("POST", "/roles", "ops", "", map[string]any{"title": "x", "required_software": []string{"Vim"}, "status": "paused"}), 422, "bad role")
	if r.field("required_software") == "" || r.field("status") == "" {
		t.Fatalf("expected field errors: %s", r.Raw)
	}

	// Everyone can read an open role; talent only sees open ones.
	for _, role := range []string{"talent", "employer", "ops"} {
		a.want(a.do("GET", "/roles/"+id, role, "", nil), 200, role+" get role")
		r = a.want(a.do("GET", "/roles", role, "", nil), 200, role+" list roles")
		if len(r.List) != 1 {
			t.Fatalf("%s list: %s", role, r.Raw)
		}
	}
	r = a.want(a.do("PUT", "/roles/"+id, "employer", "", map[string]any{"status": "closed", "nice_to_haves": []string{"cpg"}}), 200, "update")
	if r.str("status") != "closed" || fmt.Sprint(r.Body["required_software"]) != "[netsuite quickbooks]" {
		t.Fatalf("update should keep omitted fields: %s", r.Raw)
	}
	a.want(a.do("GET", "/roles/"+id, "talent", "", nil), 404, "talent closed role")
	if r = a.want(a.do("GET", "/roles", "talent", "", nil), 200, "talent list"); len(r.List) != 0 {
		t.Fatalf("talent should not list closed roles: %s", r.Raw)
	}
	if r = a.want(a.do("GET", "/roles?status=closed", "talent", "", nil), 200, "talent list closed"); len(r.List) != 0 {
		t.Fatalf("talent should not list closed roles by filter: %s", r.Raw)
	}
	if r = a.want(a.do("GET", "/roles?status=closed", "employer", "", nil), 200, "employer list closed"); len(r.List) != 1 {
		t.Fatalf("employer list closed: %s", r.Raw)
	}
	if r = a.want(a.do("GET", "/roles?company=Northwind", "ops", "", nil), 200, "filter by company"); len(r.List) != 1 {
		t.Fatalf("company filter: %s", r.Raw)
	}

	a.want(a.do("DELETE", "/roles/"+id, "employer", "", nil), 204, "delete")
	a.want(a.do("GET", "/roles/"+id, "ops", "", nil), 404, "gone")
}

func TestEmployerOnlySeesReleasedMatches(t *testing.T) {
	a := newAPI(t)
	ada := a.candidate("Ada Okafor", "ada@example.com")
	ben := a.candidate("Ben Larsen", "ben@example.com")
	cy := a.candidate("Cy Reyes", "cy@example.com")
	roleID := a.role("Senior Accountant")
	m1 := a.match(roleID, ada, 0.91)
	m2 := a.match(roleID, ben, 0.42)
	m3 := a.match(roleID, cy, 0.77)

	// Match validation.
	a.want(a.do("POST", "/matches", "ops", "", map[string]any{"role_id": roleID, "candidate_id": ada, "score": 0.5}), 409, "duplicate pair")
	r := a.want(a.do("POST", "/matches", "ops", "", map[string]any{"role_id": roleID, "candidate_id": ada, "score": 1.5, "status": "maybe"}), 422, "bad score/status")
	if r.field("score") == "" || r.field("status") == "" {
		t.Fatalf("expected field errors: %s", r.Raw)
	}
	a.want(a.do("POST", "/matches", "ops", "", map[string]any{"role_id": roleID, "candidate_id": "00000000-0000-0000-0000-000000000000", "score": 0.5}), 422, "missing candidate")

	a.want(a.do("GET", "/matches?role_id=not-a-uuid", "ops", "", nil), 422, "bad role_id filter")
	a.want(a.do("GET", "/matches?candidate_id=abc", "employer", "", nil), 422, "bad candidate_id filter")
	a.want(a.do("GET", "/matches", "talent", "ben", nil), 403, "talent with non-uuid actor")

	// Nothing is released yet: ops sees them all, employer sees none.
	if r = a.want(a.do("GET", "/matches?role_id="+roleID, "ops", "", nil), 200, "ops list"); len(r.List) != 3 {
		t.Fatalf("ops list: %s", r.Raw)
	}
	if r = a.want(a.do("GET", "/matches?role_id="+roleID, "employer", "", nil), 200, "employer list"); len(r.List) != 0 {
		t.Fatalf("employer must not see unreleased matches: %s", r.Raw)
	}
	a.want(a.do("GET", "/matches/"+m1, "employer", "", nil), 404, "employer get unreleased")
	a.want(a.do("GET", "/matches/"+m1, "ops", "", nil), 200, "ops get unreleased")

	// Ops approves two and releases the role; Ben's match stays as it was.
	const ops = "ops@example.com"
	a.want(a.do("POST", "/matches/"+m1+"/approve", "ops", ops, nil), 200, "approve ada")
	a.want(a.do("POST", "/matches/"+m3+"/approve", "ops", ops, nil), 200, "approve cy")
	r = a.want(a.do("POST", "/roles/"+roleID+"/release", "ops", ops, map[string]any{"reason": "strong fit"}), 200, "release")
	if len(r.List) != 2 || r.List[0]["released_at"] == nil || r.List[1]["released_at"] == nil {
		t.Fatalf("release should set released_at on the two approved: %s", r.Raw)
	}
	r = a.want(a.do("GET", "/matches?role_id="+roleID, "employer", "", nil), 200, "employer list after release")
	if len(r.List) != 2 || r.List[0]["id"] != m1 || r.List[0]["candidate_name"] != "Ada Okafor" || r.List[1]["id"] != m3 {
		t.Fatalf("employer should see exactly the released matches: %s", r.Raw)
	}
	a.want(a.do("GET", "/matches/"+m1, "employer", "", nil), 200, "employer get released")
	a.want(a.do("GET", "/matches/"+m2, "employer", "", nil), 404, "employer get other unreleased")
	// Employer cannot widen the filter to see unreleased rows.
	if r = a.want(a.do("GET", "/matches?status=proposed", "employer", "", nil), 200, "employer status filter"); len(r.List) != 0 {
		t.Fatalf("employer status filter leaked: %s", r.Raw)
	}
	if r = a.want(a.do("GET", "/matches", "employer", "", nil), 200, "employer unfiltered"); len(r.List) != 2 {
		t.Fatalf("employer unfiltered leaked: %s", r.Raw)
	}

	// Talent sees released matches for their own candidate only.
	a.want(a.do("GET", "/matches", "talent", "", nil), 403, "talent without actor")
	if r = a.want(a.do("GET", "/matches", "talent", ada, nil), 200, "ada matches"); len(r.List) != 1 {
		t.Fatalf("ada should see her released match: %s", r.Raw)
	}
	if r = a.want(a.do("GET", "/matches", "talent", ben, nil), 200, "ben matches"); len(r.List) != 0 {
		t.Fatalf("ben's match is unreleased: %s", r.Raw)
	}
	if r = a.want(a.do("GET", "/matches?candidate_id="+ada, "talent", ben, nil), 200, "ben asks for ada"); len(r.List) != 0 {
		t.Fatalf("talent must not see another candidate's matches: %s", r.Raw)
	}
	a.want(a.do("GET", "/matches/"+m1, "talent", ben, nil), 404, "ben gets ada's match")
	a.want(a.do("GET", "/matches/"+m1, "talent", ada, nil), 200, "ada gets own match")

	// Releasing again is a no-op with no extra audit row.
	a.want(a.do("POST", "/roles/"+roleID+"/release", "ops", ops, nil), 200, "release twice")
	a.want(a.do("POST", "/matches/"+m2+"/unrelease", "ops", ops, ""), 200, "unrelease never released")
	if n := a.count(`SELECT count(*) FROM review_events WHERE action IN ('release', 'unrelease')`); n != 2 {
		t.Fatalf("review_events rows after no-op release/unrelease = %d, want 2", n)
	}
	a.want(a.do("POST", "/roles/00000000-0000-0000-0000-000000000000/release", "ops", ops, nil), 404, "release missing")
	a.want(a.do("POST", "/matches/00000000-0000-0000-0000-000000000000/unrelease", "ops", ops, nil), 404, "unrelease missing")

	// Un-release hides it again and both actions are audited.
	a.want(a.do("POST", "/matches/"+m1+"/unrelease", "ops", ops, nil), 200, "unrelease")
	if r = a.want(a.do("GET", "/matches", "employer", "", nil), 200, "employer after unrelease"); len(r.List) != 1 || r.List[0]["id"] != m3 {
		t.Fatalf("unreleased match still visible: %s", r.Raw)
	}
	if n := a.count(`SELECT count(*) FROM review_events WHERE match_id = $1 AND actor = 'ops@example.com' AND action IN ('release','unrelease')`, m1); n != 2 {
		t.Fatalf("review_events rows = %d, want 2", n)
	}
	if n := a.count(`SELECT count(*) FROM review_events WHERE match_id = $1 AND reason = 'strong fit'`, m1); n != 1 {
		t.Fatalf("release reason not recorded")
	}

	// Ops edits; the pair is immutable, and the status is not an edit.
	r = a.want(a.do("PUT", "/matches/"+m2, "ops", "", map[string]any{"score": 0.6, "explanation": "solid", "breakdown": map[string]any{"filters": "pass"}}), 200, "update")
	if r.str("status") != "proposed" || r.Body["score"] != 0.6 || r.str("explanation") != "solid" {
		t.Fatalf("update: %s", r.Raw)
	}
	r = a.want(a.do("PUT", "/matches/"+m2, "ops", "", map[string]any{"explanation": nil}), 200, "clear explanation")
	if r.str("explanation") != "" || r.Body["score"] != 0.6 {
		t.Fatalf("null explanation should clear only that field: %s", r.Raw)
	}
	a.want(a.do("PUT", "/matches/"+m2, "ops", "", map[string]any{"candidate_id": ada}), 422, "change pair")
	a.want(a.do("PUT", "/matches/"+m2, "ops", "", map[string]any{"status": "approved"}), 422, "change status")

	// A match with review history cannot be hard-deleted; one without can.
	a.want(a.do("DELETE", "/matches/"+m1, "ops", "", nil), 409, "delete audited match")
	a.want(a.do("DELETE", "/matches/"+m2, "ops", "", nil), 204, "delete match")
	a.want(a.do("GET", "/matches/"+m2, "ops", "", nil), 404, "gone")
}
