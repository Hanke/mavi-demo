package server

import (
	"slices"
	"strings"
	"testing"
)

const (
	opsActor  = "ops@example.com"
	leadActor = "lead@example.com"
)

// reviewPool is a role with matches as a matching run leaves them: some in
// the queue (pending_review) and the rest in reserve (proposed).
type reviewPool struct {
	*api
	role    string
	matches map[string]string // candidate name -> match id
}

func newReviewPool(t *testing.T) *reviewPool {
	a := newAPI(t)
	return &reviewPool{api: a, role: a.role("Controller"), matches: map[string]string{}}
}

// add gives the role a match for a new candidate, in the queue or in reserve.
func (p *reviewPool) add(name string, score float64, queued bool) string {
	p.t.Helper()
	cand := p.candidate(name, strings.ToLower(name)+"@example.com")
	p.do("PUT", "/candidates/"+cand+"/availability", "ops", "", workHours("America/Chicago", "09:00", "17:00", 40, "2020-01-01"))
	status := "proposed"
	if queued {
		status = "pending_review"
	}
	r := p.want(p.do("POST", "/matches", "ops", "", map[string]any{"role_id": p.role, "candidate_id": cand, "score": score, "status": status}), 201, "match "+name)
	p.matches[name] = r.str("id")
	return cand
}

// act is a review action on name's match, as actor.
func (p *reviewPool) act(action, name, actor string, body any) resp {
	p.t.Helper()
	return p.do("POST", "/matches/"+p.matches[name]+"/"+action, "ops", actor, body)
}

func (p *reviewPool) release(actor string, body any) resp {
	p.t.Helper()
	return p.do("POST", "/roles/"+p.role+"/release", "ops", actor, body)
}

// queue is the review queue as "pending | approved | next", by name.
func (p *reviewPool) queue() string {
	p.t.Helper()
	r := p.want(p.do("GET", "/roles/"+p.role+"/review-queue", "ops", "", nil), 200, "review queue")
	next := "nobody"
	if m, ok := r.Body["next"].(map[string]any); ok {
		next = m["candidate_name"].(string)
	}
	return names(r.Body["pending"]) + " | " + names(r.Body["approved"]) + " | " + next
}

// employerSees is the names on the employer's view of the role.
func (p *reviewPool) employerSees() string {
	p.t.Helper()
	r := p.want(p.do("GET", "/matches?role_id="+p.role, "employer", "", nil), 200, "employer matches")
	out := make([]string, len(r.List))
	for i, m := range r.List {
		if m["released_at"] == nil {
			p.t.Fatalf("the employer was shown an unreleased match: %s", r.Raw)
		}
		out[i] = m["candidate_name"].(string)
	}
	return strings.Join(out, ", ")
}

// trail is the role's audit trail, one "action candidate by actor" each,
// with the candidate a swap brought in.
func (p *reviewPool) trail() []string {
	p.t.Helper()
	r := p.want(p.do("GET", "/roles/"+p.role+"/review-events", "ops", "", nil), 200, "review events")
	out := make([]string, len(r.List))
	for i, e := range r.List {
		out[i] = e["action"].(string) + " " + e["candidate_name"].(string) + " by " + e["actor"].(string)
		if in, ok := e["replacement_candidate_name"].(string); ok {
			out[i] += " for " + in
		}
		if e["role_id"] != p.role || e["created_at"] == nil {
			p.t.Fatalf("event %d: %v", i, e)
		}
	}
	return out
}

// names is the candidates of a list of matches, in order.
func names(list any) string {
	items, _ := list.([]any)
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.(map[string]any)["candidate_name"].(string)
	}
	return strings.Join(out, ", ")
}

// The whole of human validation for one role: ops reads the queue, approves
// and swaps, and releases; the release is refused unless exactly two are
// approved; the employer sees those two and nobody else; and review_events
// says who did each thing.
func TestReviewApproveSwapRelease(t *testing.T) {
	p := newReviewPool(t)
	p.add("Ada", 0.9, true)
	ben := p.add("Ben", 0.8, true)
	p.add("Cy", 0.7, false)
	p.add("Dan", 0.65, false)
	p.add("Eve", 0.5, false) // below the minimum: never brought in by a swap

	if got := p.queue(); got != "Ada, Ben |  | Cy" {
		t.Fatalf("queue: %s", got)
	}
	p.want(p.do("GET", "/roles/"+p.role+"/review-queue", "employer", "", nil), 403, "employer reads the queue")
	p.want(p.do("GET", "/roles/"+p.role+"/review-queue", "talent", "", nil), 403, "talent reads the queue")
	p.want(p.do("GET", "/roles/00000000-0000-0000-0000-000000000000/review-queue", "ops", "", nil), 404, "queue of no role")

	// Nobody approved: nothing to release.
	if r := p.want(p.release(opsActor, nil), 409, "release with none approved"); !strings.Contains(r.str("error"), "exactly 2 approved") || !strings.Contains(r.str("error"), "has 0") {
		t.Fatalf("release with none approved: %s", r.Raw)
	}

	// A decision names who made it, or is not made.
	p.want(p.act("approve", "Ada", "", nil), 403, "approve without an actor")
	p.want(p.release("", nil), 403, "release without an actor")
	if n := p.count(`SELECT count(*) FROM review_events`); n != 0 {
		t.Fatalf("%d events before any decision", n)
	}
	p.want(p.act("approve", "Ada", opsActor, map[string]any{"because": "x"}), 400, "unknown field")
	p.want(p.do("POST", "/matches/00000000-0000-0000-0000-000000000000/approve", "ops", opsActor, nil), 404, "approve no match")
	p.want(p.do("POST", "/matches/not-a-uuid/swap", "ops", opsActor, nil), 404, "swap a bad id")

	// Approving is not releasing, and approving twice is one decision.
	r := p.want(p.act("approve", "Ada", opsActor, map[string]any{"reason": "ran close at a manufacturer"}), 200, "approve Ada")
	if r.str("status") != "approved" || r.Body["released_at"] != nil {
		t.Fatalf("approve: %s", r.Raw)
	}
	p.want(p.act("approve", "Ada", opsActor, nil), 200, "approve Ada again")
	if n := p.count(`SELECT count(*) FROM review_events WHERE match_id = $1`, p.matches["Ada"]); n != 1 {
		t.Fatalf("approving twice recorded %d events", n)
	}
	if r := p.want(p.release(opsActor, nil), 409, "release with one approved"); !strings.Contains(r.str("error"), "has 1") {
		t.Fatalf("release with one approved: %s", r.Raw)
	}
	if got := p.employerSees(); got != "" {
		t.Fatalf("the employer sees %q before any release", got)
	}

	// Ben is swapped out for the next ranked, who waits for a decision.
	r = p.want(p.act("swap", "Ben", opsActor, map[string]any{"reason": "took another offer"}), 200, "swap Ben")
	out, in := r.Body["swapped"].(map[string]any), r.Body["replacement"].(map[string]any)
	if out["candidate_name"] != "Ben" || out["status"] != "swapped" || in["candidate_name"] != "Cy" || in["status"] != "pending_review" {
		t.Fatalf("swap: %s", r.Raw)
	}
	if got := p.queue(); got != "Cy | Ada | Dan" {
		t.Fatalf("queue after the swap: %s", got)
	}
	p.want(p.act("swap", "Ben", opsActor, nil), 409, "swap Ben again")
	p.want(p.act("approve", "Ben", opsActor, nil), 409, "approve the candidate swapped out")
	p.want(p.act("swap", "Dan", opsActor, nil), 409, "swap a candidate who is not in review")

	// Three approved is not two either.
	p.want(p.act("approve", "Cy", opsActor, nil), 200, "approve Cy")
	p.want(p.act("approve", "Dan", opsActor, nil), 200, "approve Dan, from reserve")
	if r := p.want(p.release(opsActor, nil), 409, "release with three approved"); !strings.Contains(r.str("error"), "has 3") {
		t.Fatalf("release with three approved: %s", r.Raw)
	}
	if got := p.employerSees(); got != "" {
		t.Fatalf("a refused release showed the employer %q", got)
	}

	// Only Eve is left in reserve, below the minimum: there is nobody to
	// swap Dan for, so nothing changes. Rejecting takes the approval back.
	if r := p.want(p.act("swap", "Dan", opsActor, nil), 409, "swap with nobody in reserve"); !strings.Contains(r.str("error"), "no next ranked candidate") {
		t.Fatalf("swap with nobody in reserve: %s", r.Raw)
	}
	if got := p.queue(); got != " | Ada, Cy, Dan | nobody" {
		t.Fatalf("queue after a refused swap: %s", got)
	}
	if r := p.want(p.act("reject", "Dan", opsActor, map[string]any{"reason": "two is the promise"}), 200, "reject Dan"); r.str("status") != "rejected" {
		t.Fatalf("reject: %s", r.Raw)
	}
	p.want(p.act("reject", "Dan", opsActor, nil), 200, "reject Dan again")
	p.want(p.act("approve", "Dan", opsActor, nil), 409, "approve the candidate rejected")

	// Exactly two approved: released, by whoever releases.
	r = p.want(p.release(leadActor, map[string]any{"reason": "shortlist agreed"}), 200, "release")
	if len(r.List) != 2 || r.List[0]["candidate_name"] != "Ada" || r.List[1]["candidate_name"] != "Cy" || r.List[0]["released_at"] == nil || r.List[1]["released_at"] == nil {
		t.Fatalf("release: %s", r.Raw)
	}

	// The employer sees those two and nothing else, however they ask.
	if got := p.employerSees(); got != "Ada, Cy" {
		t.Fatalf("the employer sees %q", got)
	}
	if r := p.want(p.do("GET", "/matches", "employer", "", nil), 200, "employer, every role"); len(r.List) != 2 {
		t.Fatalf("employer, every role: %s", r.Raw)
	}
	for _, status := range []string{"proposed", "pending_review", "rejected", "swapped"} {
		if r := p.want(p.do("GET", "/matches?status="+status, "employer", "", nil), 200, "employer by status"); len(r.List) != 0 {
			t.Fatalf("employer asking for %s: %s", status, r.Raw)
		}
	}
	for _, name := range []string{"Ben", "Dan", "Eve"} {
		p.want(p.do("GET", "/matches/"+p.matches[name], "employer", "", nil), 404, "employer reads "+name)
	}
	if r := p.want(p.do("GET", "/roles/"+p.role+"/match-status", "employer", "", nil), 200, "match status"); r.str("status") != "ready" {
		t.Fatalf("match status after the release: %s", r.Raw)
	}

	// Released is settled: releasing again records nothing, and a released
	// candidate is not swapped or rejected from under the employer.
	p.want(p.release(opsActor, nil), 200, "release again")
	p.want(p.act("swap", "Ada", opsActor, nil), 409, "swap a released match")
	p.want(p.act("reject", "Ada", opsActor, nil), 409, "reject a released match")

	// The audit trail: each decision once, in order, with who made it.
	want := []string{
		"approve Ada by " + opsActor,
		"swap Ben by " + opsActor + " for Cy",
		"approve Cy by " + opsActor,
		"approve Dan by " + opsActor,
		"reject Dan by " + opsActor,
		"release Ada by " + leadActor,
		"release Cy by " + leadActor,
	}
	got := p.trail()
	slices.Sort(got[len(got)-2:]) // the two of a release are recorded together
	if !slices.Equal(got, want) {
		t.Fatalf("audit trail:\n got %q\nwant %q", got, want)
	}
	if n := p.count(`SELECT count(*) FROM review_events WHERE action = 'swap' AND reason = 'took another offer' AND metadata->>'from' = 'pending_review' AND metadata->>'replacement_match_id' = $1`, p.matches["Cy"]); n != 1 {
		t.Fatal("the swap event does not carry its reason, what the match was and who came in")
	}
	if n := p.count(`SELECT count(*) FROM review_events WHERE action = 'release' AND reason = 'shortlist agreed'`); n != 2 {
		t.Fatalf("release events with the reason = %d, want 2", n)
	}
	if n := p.count(`SELECT count(*) FROM review_events WHERE actor = ''`); n != 0 {
		t.Fatalf("%d events without an actor", n)
	}
	p.want(p.do("GET", "/roles/"+p.role+"/review-events", "employer", "", nil), 403, "employer reads the audit trail")
	p.want(p.do("GET", "/roles/00000000-0000-0000-0000-000000000000/review-events", "ops", "", nil), 404, "audit trail of no role")
	if r := p.want(p.do("GET", "/roles/"+p.role+"/review-events?limit=2&offset=1", "ops", "", nil), 200, "a page of the trail"); len(r.List) != 2 || r.List[0]["action"] != "swap" {
		t.Fatalf("a page of the trail: %s", r.Raw)
	}

	// A decision cannot be written around the audit trail.
	if r := p.want(p.do("PUT", "/matches/"+p.matches["Eve"], "ops", "", map[string]any{"status": "approved"}), 422, "approve by edit"); r.field("status") == "" {
		t.Fatalf("approve by edit: %s", r.Raw)
	}
	cand := p.candidate("Fay", "fay@example.com")
	p.do("PUT", "/candidates/"+cand+"/availability", "ops", "", workHours("America/Chicago", "09:00", "17:00", 40, "2020-01-01"))
	if r := p.want(p.do("POST", "/matches", "ops", "", map[string]any{"role_id": p.role, "candidate_id": cand, "score": 0.9, "status": "approved"}), 422, "create approved"); r.field("status") == "" {
		t.Fatalf("create approved: %s", r.Raw)
	}

	// Withdrawing one is recorded too, and the release puts it back.
	p.want(p.act("unrelease", "Ada", "", nil), 403, "withdraw without an actor")
	if r := p.want(p.act("unrelease", "Ada", opsActor, map[string]any{"reason": "reference check"}), 200, "withdraw Ada"); r.Body["released_at"] != nil || r.str("status") != "approved" {
		t.Fatalf("withdraw: %s", r.Raw)
	}
	if got := p.employerSees(); got != "Cy" {
		t.Fatalf("the employer sees %q after Ada is withdrawn", got)
	}
	p.want(p.release(opsActor, nil), 200, "release after a withdrawal")
	if got := p.employerSees(); got != "Ada, Cy" {
		t.Fatalf("the employer sees %q after the second release", got)
	}
	if got := p.trail(); len(got) != 9 || got[7] != "unrelease Ada by "+opsActor || got[8] != "release Ada by "+opsActor {
		t.Fatalf("audit trail after a withdrawal: %q", got)
	}

	// The trail outlives what it is about: nothing with history is deleted.
	p.want(p.do("DELETE", "/matches/"+p.matches["Ben"], "ops", "", nil), 409, "delete a reviewed match")
	p.want(p.do("DELETE", "/candidates/"+ben, "ops", "", nil), 409, "delete a reviewed candidate")
	p.want(p.do("DELETE", "/roles/"+p.role, "ops", "", nil), 409, "delete a reviewed role")
	if got := p.trail(); len(got) != 9 {
		t.Fatalf("audit trail after refused deletes: %q", got)
	}
}

// The next ranked candidate is the best of the reserve who can still be put
// forward: by score, and not someone who has left the pool.
func TestSwapBringsInTheNextRanked(t *testing.T) {
	p := newReviewPool(t)
	p.add("Ada", 0.9, true)
	ben := p.add("Ben", 0.95, false)
	p.add("Cy", 0.7, false)
	p.add("Dan", 0.8, false)
	p.want(p.do("PUT", "/candidates/"+ben, "ops", "", map[string]any{"status": "archived"}), 200, "archive Ben")

	if got := p.queue(); got != "Ada |  | Dan" {
		t.Fatalf("queue: %s", got)
	}
	r := p.want(p.act("swap", "Ada", opsActor, nil), 200, "swap Ada")
	if in := r.Body["replacement"].(map[string]any); in["candidate_name"] != "Dan" {
		t.Fatalf("swap brought in %v", in["candidate_name"])
	}
	// A candidate rejected from the reserve is not brought in either.
	p.want(p.act("reject", "Cy", opsActor, nil), 200, "reject Cy")
	if got := p.queue(); got != "Dan |  | nobody" {
		t.Fatalf("queue after the swap: %s", got)
	}
	p.want(p.act("swap", "Dan", opsActor, nil), 409, "swap with nobody left")
}

// A match released before approval was required is not approved. The role is
// not released around it, since the employer would see three.
func TestReleaseRefusesWhileAnUnapprovedMatchIsReleased(t *testing.T) {
	p := newReviewPool(t)
	p.add("Ada", 0.9, true)
	p.add("Ben", 0.8, true)
	p.add("Cy", 0.7, false)
	p.exec(`UPDATE matches SET released_at = now() WHERE id = $1`, p.matches["Cy"])
	p.want(p.act("approve", "Ada", opsActor, nil), 200, "approve Ada")
	p.want(p.act("approve", "Ben", opsActor, nil), 200, "approve Ben")

	if r := p.want(p.release(opsActor, nil), 409, "release around Cy"); !strings.Contains(r.str("error"), "not approved") {
		t.Fatalf("release around Cy: %s", r.Raw)
	}
	if got := p.employerSees(); got != "Cy" {
		t.Fatalf("a refused release changed what the employer sees: %q", got)
	}
	p.want(p.act("unrelease", "Cy", opsActor, nil), 200, "withdraw Cy")
	p.want(p.release(opsActor, nil), 200, "release")
	if got := p.employerSees(); got != "Ada, Ben" {
		t.Fatalf("the employer sees %q", got)
	}
}
