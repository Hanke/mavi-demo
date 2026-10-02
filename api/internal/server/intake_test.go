package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/colehanke/mavi-demo/api/internal/aiclient"
	"github.com/colehanke/mavi-demo/api/internal/tasks"
)

const pastedJD = "Senior Accountant at Northwind Traders.\nActive CPA required. 5+ years. NetSuite. Nice to have: SaaS experience."

// parsedJD is what the stub parser makes of pastedJD, as the AI service would
// send it.
func parsedJD(t *testing.T, requirements string) aiclient.ParsedJD {
	t.Helper()
	company := "Northwind Traders"
	out := aiclient.ParsedJD{Company: &company, RequirementsJSON: json.RawMessage(requirements), Provider: "stub"}
	if err := json.Unmarshal(out.RequirementsJSON, &out.Requirements); err != nil {
		t.Fatal(err)
	}
	return out
}

const seniorAccountant = `{"title":"Senior Accountant","required_certifications":["cpa_us"],"required_software":["netsuite"],
	"min_years_experience":5,"must_haves":["Active CPA","5+ years of experience","NetSuite"],"nice_to_haves":["SaaS experience"],
	"timezone":"America/Chicago","min_overlap_hours":4,"hours_per_week":40,"starts_on":"2026-11-02"}`

// Intake stores the raw JD, what the parser extracted and the embedding,
// answers with the role so the employer sees the requirements, and queues the
// matching run.
func TestRoleIntakeStoresParsedRoleAndQueuesMatching(t *testing.T) {
	var asked string
	a := newAPIWith(t, stub{parseJD: func(text string) (aiclient.ParsedJD, error) {
		asked = text
		return parsedJD(t, seniorAccountant), nil
	}})

	a.want(a.do("POST", "/roles/intake", "talent", "x", map[string]any{"description": pastedJD}), 403, "talent intake")
	r := a.want(a.do("POST", "/roles/intake", "employer", "", map[string]any{"description": pastedJD}), 201, "intake")
	if asked != pastedJD {
		t.Fatalf("the parser was sent %q", asked)
	}

	// The response shows the employer what was extracted.
	role, _ := r.Body["role"].(map[string]any)
	id, _ := role["id"].(string)
	want := map[string]string{
		"title": `"Senior Accountant"`, "company": `"Northwind Traders"`, "status": `"open"`,
		"must_haves":              `["Active CPA","5+ years of experience","NetSuite"]`,
		"nice_to_haves":           `["SaaS experience"]`,
		"required_certifications": `["cpa_us"]`, "required_software": `["netsuite"]`,
		"min_years_experience": `5`, "timezone": `"America/Chicago"`, "min_overlap_hours": `4`, "hours_per_week": `40`,
		"starts_on": `"2026-11-02"`, "embedding_model": `"stub"`,
	}
	for field, wantJSON := range want {
		if got, _ := json.Marshal(role[field]); string(got) != wantJSON {
			t.Errorf("role.%s = %s, want %s", field, got, wantJSON)
		}
	}
	if role["description"] != pastedJD || role["embedded_at"] == nil {
		t.Fatalf("role: %s", r.Raw)
	}
	if req, _ := role["requirements"].(map[string]any); req["title"] != "Senior Accountant" || req["starts_on"] != "2026-11-02" {
		t.Fatalf("requirements should be the whole extraction: %s", r.Raw)
	}

	// The row holds the raw JD, both lists and the vector.
	if n := a.count(`SELECT count(*) FROM roles WHERE id = $1 AND description = $2 AND embedding IS NOT NULL
		AND must_haves = '["Active CPA","5+ years of experience","NetSuite"]' AND nice_to_haves = '["SaaS experience"]'
		AND requirements->>'title' = 'Senior Accountant'`, id, pastedJD); n != 1 {
		t.Fatalf("stored role does not hold the JD, the lists and the embedding: %s", r.Raw)
	}

	// One matching job for the role, returned in the response; embedded in
	// the request, so no embedding job.
	job, _ := r.Body["matching_job"].(map[string]any)
	payload, _ := job["payload"].(map[string]any)
	if job["kind"] != tasks.KindMatchRole || job["status"] != "queued" || payload["role_id"] != id {
		t.Fatalf("matching_job: %s", r.Raw)
	}
	if n := a.count(`SELECT count(*) FROM jobs WHERE kind = $1 AND payload->>'role_id' = $2 AND status = 'queued'`, tasks.KindMatchRole, id); n != 1 {
		t.Fatalf("want 1 queued %s job, got %d", tasks.KindMatchRole, n)
	}
	if n := a.count(`SELECT count(*) FROM jobs WHERE kind = $1`, tasks.KindEmbedRole); n != 0 {
		t.Fatalf("an embedded role queued %d %s jobs", n, tasks.KindEmbedRole)
	}

	// The role is an ordinary role afterwards.
	if got := a.want(a.do("GET", "/roles/"+id, "employer", "", nil), 200, "get"); got.str("title") != "Senior Accountant" {
		t.Fatalf("get: %s", got.Raw)
	}

	// The request's title and company win over the parser's.
	r = a.want(a.do("POST", "/roles/intake", "ops", "", map[string]any{"description": pastedJD, "title": " Lead Accountant ", "company": "Acme"}), 201, "intake with overrides")
	if role, _ = r.Body["role"].(map[string]any); role["title"] != "Lead Accountant" || role["company"] != "Acme" {
		t.Fatalf("overrides: %s", r.Raw)
	}
}

// Values the columns cannot hold are left out rather than failing the intake;
// a JD with no title needs one from the employer.
func TestRoleIntakeSoftFieldsAndTitle(t *testing.T) {
	a := newAPIWith(t, stub{parseJD: func(string) (aiclient.ParsedJD, error) {
		out := parsedJD(t, `{"must_haves":["Keeps the books"],"timezone":"Mars/Olympus","min_overlap_hours":4,"hours_per_week":0,"min_years_experience":0}`)
		out.Company = nil
		return out, nil
	}})

	r := a.want(a.do("POST", "/roles/intake", "employer", "", map[string]any{"description": "Keeps the books."}), 422, "no title anywhere")
	if r.field("title") == "" {
		t.Fatalf("the 422 should name title: %s", r.Raw)
	}
	if n := a.count(`SELECT count(*) FROM roles`) + a.count(`SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("a refused intake left %d rows", n)
	}

	r = a.want(a.do("POST", "/roles/intake", "employer", "", map[string]any{"description": "Keeps the books.", "title": "Bookkeeper"}), 201, "title supplied")
	role, _ := r.Body["role"].(map[string]any)
	for _, field := range []string{"company", "timezone", "min_overlap_hours", "hours_per_week", "min_years_experience", "starts_on"} {
		if v, ok := role[field]; !ok || v != nil {
			t.Errorf("role.%s = %v, want null", field, v)
		}
	}
}

// A parse that fails stores nothing; an embedding that fails leaves the role
// and its matching job in place and hands the embedding to its own job.
func TestRoleIntakeFailures(t *testing.T) {
	silence(t)
	var parseErr error
	a := newAPIWith(t, stub{parseJD: func(string) (aiclient.ParsedJD, error) { return aiclient.ParsedJD{}, parseErr }})

	a.want(a.do("POST", "/roles/intake", "employer", "", map[string]any{}), 422, "missing description")
	r := a.want(a.do("POST", "/roles/intake", "employer", "", map[string]any{"description": "  "}), 422, "blank description")
	if r.field("description") == "" {
		t.Fatalf("the 422 should name description: %s", r.Raw)
	}
	a.want(a.do("POST", "/roles/intake", "employer", "", map[string]any{"description": strings.Repeat("x", maxJDChars+1)}), 422, "too long")
	a.want(a.do("POST", "/roles/intake", "employer", "", map[string]any{"description": "x", "status": "open"}), 400, "unknown field")

	parseErr = errors.New("connection refused")
	a.want(a.do("POST", "/roles/intake", "employer", "", map[string]any{"description": pastedJD}), 503, "AI service down")
	if n := a.count(`SELECT count(*) FROM roles`) + a.count(`SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("failed intakes left %d rows", n)
	}

	// The parser works, the embedder does not.
	b := newAPIWith(t, stub{
		parseJD:  func(string) (aiclient.ParsedJD, error) { return parsedJD(t, seniorAccountant), nil },
		embedErr: errors.New("connection refused"),
	})
	r = b.want(b.do("POST", "/roles/intake", "employer", "", map[string]any{"description": pastedJD}), 201, "intake without embedding")
	role, _ := r.Body["role"].(map[string]any)
	if role["embedded_at"] != nil {
		t.Fatalf("the role should not claim an embedding: %s", r.Raw)
	}
	for _, kind := range []string{tasks.KindEmbedRole, tasks.KindMatchRole} {
		if n := b.count(`SELECT count(*) FROM jobs WHERE kind = $1 AND payload->>'role_id' = $2 AND status = 'queued'`, kind, role["id"]); n != 1 {
			t.Fatalf("want 1 queued %s job, got %d", kind, n)
		}
	}

	// Ids the API's taxonomy does not have are the services disagreeing, not
	// the employer's mistake.
	c := newAPIWith(t, stub{parseJD: func(string) (aiclient.ParsedJD, error) {
		return parsedJD(t, `{"title":"Editor","required_software":["vim"]}`), nil
	}})
	c.want(c.do("POST", "/roles/intake", "employer", "", map[string]any{"description": pastedJD}), 500, "unknown taxonomy id")
	if n := c.count(`SELECT count(*) FROM roles`); n != 0 {
		t.Fatalf("an intake with unknown ids stored %d roles", n)
	}
}
