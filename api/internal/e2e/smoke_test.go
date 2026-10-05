//go:build smoke

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/auth"
	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/dbtest"
)

// smokeJD is the job description the employer pastes. It is written for the
// key-free LLM the smoke stack runs (LLM_PROVIDER=fake, ai/app/fake.py),
// which reads the bullets under "Requirements" as the must-haves and scores
// a resume on the taxonomy terms and the wording it shares with the role.
const smokeJD = `Senior Accountant (Remote)
Northwind Software | Finance & Accounting | Full-time

Northwind Software sells subscription billing software to mid-sized companies. Our accounting team runs the monthly close in NetSuite, and we are hiring a Senior Accountant to own the month-end close and the balance sheet reconciliations.

Responsibilities
- Own the month-end close: journal entries, accruals, prepaids and fixed assets in NetSuite.
- Reconcile the balance sheet accounts each month and prepare the schedules for the annual audit.
- Reconcile deferred revenue to the billing system.

Requirements
- CPA or an equivalent qualification (ACA, ACCA).
- 5+ years of accounting experience.
- NetSuite as a daily user.
- Advanced Excel.

Preferred
- BlackLine for account reconciliations.
- Experience at a SaaS company.
`

// smokeTalent is who signs up, each with a resume from infra/fixtures/resumes.
// The first two are what the role asks for. The third meets the must-haves
// on paper and is a weaker fit, so she is matched and not one of the two.
// The fourth has neither the licence nor the years, and is never matched.
var smokeTalent = []struct{ name, resume string }{
	{"Daniel Okafor", "senior_accountant_cpa_netsuite"},
	{"Hannah Whitaker", "financial_controller_aca_uk"},
	{"Beatriz Almeida", "fractional_cfo"},
	{"Maya Lindqvist", "staff_accountant_early_career"},
}

// The whole loop, over HTTP, against a running stack: candidates sign up and
// upload their resumes, an employer pastes a job description, the pipeline
// puts the strongest candidates in front of ops, ops approves two and
// releases the role, and the employer sees those two.
//
// `make smoke` is how it is run: it starts a copy of the compose stack with
// an empty database and the key-free providers, and points SMOKE_API_URL at
// it. The test counts on that: on a database that already holds candidates,
// they are in the pool too.
func TestSmoke(t *testing.T) {
	base := os.Getenv("SMOKE_API_URL")
	if base == "" {
		t.Fatal("SMOKE_API_URL is not set; `make smoke` starts the stack and sets it")
	}
	c := newClient(t, base)

	// The API is up and reaches Postgres and the AI service.
	health := send[contract.HealthResponse](c, persona{}, "GET", "/health", nil, 200)
	if health.Checks["postgres"] != "ok" || health.Checks["ai"] != "ok" {
		t.Fatalf("health: %+v", health)
	}

	// Talent: sign up, upload a resume, say when you can work. The upload is
	// parsed into a profile and embedded by the worker.
	ids := map[string]string{}
	for _, p := range smokeTalent {
		pdf, err := os.ReadFile(filepath.Join(dbtest.RepoRoot(), "infra", "fixtures", "resumes", p.resume+".pdf"))
		if err != nil {
			t.Fatal(err)
		}
		id := send[contract.Candidate](c, persona{role: auth.Talent}, "POST", "/candidates", map[string]any{"full_name": p.name}, 201).ID
		ids[p.name] = id
		send[contract.Job](c, talent(id), "POST", "/candidates/"+id+"/resume", pdf, 202)
		send[contract.WorkAvailability](c, talent(id), "PUT", "/candidates/"+id+"/availability", map[string]any{
			"timezone": "America/New_York", "work_start": "09:00", "work_end": "17:00", "hours_per_week": 40, "available_from": "2026-01-01",
		}, 201)
	}
	for _, p := range smokeTalent {
		id := ids[p.name]
		c.eventually(p.name+"'s resume to be parsed and embedded", 2*time.Minute, func() (bool, string) {
			if ok, state := c.finished(send[contract.Job](c, talent(id), "GET", "/candidates/"+id+"/resume/job", nil, 200)); !ok {
				return false, state
			}
			return send[contract.Profile](c, talent(id), "GET", "/candidates/"+id+"/profile", nil, 200).EmbeddedAt != nil, "the profile is not embedded"
		})
	}

	// Employer: paste the job description. The role comes back as it was
	// read, with the matching run queued.
	intake := send[contract.RoleIntake](c, employer, "POST", "/roles/intake", map[string]any{"description": smokeJD}, 201)
	role := intake.Role.ID
	t.Logf("role %s: %q, requires %v and %v", role, intake.Role.Title, intake.Role.RequiredCertifications, intake.Role.RequiredSoftware)

	// Ops: the run finishes with enough candidates to put forward. A run that
	// had to wait for an embedding queues another behind it, so what is waited
	// for is the run's outcome, not the first job.
	var status contract.RoleMatchStatus
	c.eventually("the matching run", 3*time.Minute, func() (bool, string) {
		job := send[contract.Job](c, ops, "GET", fmt.Sprintf("/jobs/%d", intake.MatchingJob.ID), nil, 200)
		c.finished(job)
		status = send[contract.RoleMatchStatus](c, ops, "GET", "/roles/"+role+"/match-status", nil, 200)
		return status.Run != nil, "no run has an outcome; the first match_role job is " + string(job.Status)
	})
	t.Logf("matching run: %s", funnel(*status.Run))
	if status.Status != contract.RoleMatchStateInReview {
		t.Fatalf("the role is %s (%v) after its matching run, want in_review", status.Status, status.Run.AttentionReason)
	}

	// The two strong candidates are in the review queue.
	queue := send[contract.ReviewQueue](c, ops, "GET", "/roles/"+role+"/review-queue", nil, 200)
	t.Logf("review queue: %s", summary(queue.Pending))
	var chosen []contract.Match
	for _, name := range []string{"Daniel Okafor", "Hannah Whitaker"} {
		for _, m := range queue.Pending {
			if m.CandidateID == ids[name] {
				chosen = append(chosen, m)
			}
		}
	}
	if len(chosen) != 2 {
		t.Fatalf("the review queue holds %q; want Daniel Okafor and Hannah Whitaker in it", summary(queue.Pending))
	}
	// Beatriz met the must-haves, so she was ranked; Maya did not, and was not.
	all := send[[]contract.Match](c, ops, "GET", "/matches?role_id="+role, nil, 200)
	matched := map[string]bool{}
	for _, m := range all {
		matched[m.CandidateID] = true
	}
	if !matched[ids["Beatriz Almeida"]] || matched[ids["Maya Lindqvist"]] {
		t.Fatalf("the role's matches are %q; want Beatriz Almeida among them and Maya Lindqvist not", summary(all))
	}

	// Nothing reaches the employer until ops approves two and releases.
	if seen := send[[]contract.Match](c, employer, "GET", "/matches?role_id="+role, nil, 200); len(seen) != 0 {
		t.Fatalf("the employer sees %q before the release", names(seen))
	}
	for _, m := range chosen {
		send[contract.Match](c, ops, "POST", "/matches/"+m.ID+"/approve", nil, 200)
	}
	send[[]contract.Match](c, ops, "POST", "/roles/"+role+"/release", nil, 200)

	// Employer: the two approved, and nobody else.
	seen := send[[]contract.Match](c, employer, "GET", "/matches?role_id="+role, nil, 200)
	if len(seen) != 2 || len(all) < 3 {
		t.Fatalf("the employer sees %q of the role's %d matches, want two of at least three", names(seen), len(all))
	}
	for _, m := range seen {
		if (m.ID != chosen[0].ID && m.ID != chosen[1].ID) || m.ReleasedAt == nil {
			t.Errorf("the employer was shown %s (released at %v), whom ops did not release", m.CandidateName, m.ReleasedAt)
		}
	}
	if s := send[contract.RoleMatchStatus](c, employer, "GET", "/roles/"+role+"/match-status", nil, 200); s.Status != contract.RoleMatchStateReady {
		t.Errorf("the employer is told the role is %s, want ready", s.Status)
	}

	// Talent: a released candidate sees their match; one who was not released sees nothing.
	if mine := send[[]contract.Match](c, talent(ids["Daniel Okafor"]), "GET", "/matches", nil, 200); len(mine) != 1 || mine[0].RoleID != role {
		t.Errorf("Daniel Okafor sees %d matches, want the one for this role", len(mine))
	}
	if hers := send[[]contract.Match](c, talent(ids["Beatriz Almeida"]), "GET", "/matches", nil, 200); len(hers) != 0 {
		t.Errorf("Beatriz Almeida sees %d matches, want none: hers was not released", len(hers))
	}
}
