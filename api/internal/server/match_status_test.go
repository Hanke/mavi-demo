package server

import "testing"

// Where a role's matching stands: ops is told that a run needs attention,
// why, and which must-have eliminated the most candidates; an employer is
// told "in review", and nothing of the run, until two matches are released.
func TestRoleMatchStatus(t *testing.T) {
	p := newFilterPool(t)
	a := newAPIOn(t, p.pool, stub{}, 0)
	roleID := p.role("Anyone")

	for _, persona := range []string{"ops", "employer"} {
		r := a.want(a.do("GET", "/roles/"+roleID+"/match-status", persona, "", nil), 200, persona+" before any run")
		if r.str("status") != "in_review" || r.Body["run"] != nil || r.Body["released"] != float64(0) || r.str("role_id") != roleID {
			t.Fatalf("%s before any run: %s", persona, r.Raw)
		}
	}

	// A run of the filters alone has no outcome and is not the role's status.
	strict := a.want(a.do("POST", "/roles", "employer", "", map[string]any{
		"title": "Strict", "required_certifications": []string{"CPA"}, "required_software": []string{"NetSuite"}, "min_years_experience": 40,
	}), 201, "create role").str("id")
	run := a.want(a.do("POST", "/roles/"+strict+"/filter-runs", "ops", "", nil), 201, "run filters")
	if run.Body["match_status"] != nil || run.Body["attention_reason"] != nil {
		t.Fatalf("a run of the filters alone has an outcome: %s", run.Raw)
	}
	top, _ := run.Body["top_filter"].(map[string]any)
	if top == nil || top["excluded"].(float64) < 1 {
		t.Fatalf("want the must-have that excluded the most: %s", run.Raw)
	}
	for _, s := range run.Body["stages"].([]any) {
		stage := s.(map[string]any)
		if stage["filter"] != "profile" && stage["excluded"].(float64) > top["excluded"].(float64) {
			t.Fatalf("%v excluded more than top_filter: %s", stage["filter"], run.Raw)
		}
	}
	if r := a.want(a.do("GET", "/roles/"+strict+"/match-status", "ops", "", nil), 200, "ops after filters alone"); r.str("status") != "in_review" || r.Body["run"] != nil {
		t.Fatalf("ops after a run of the filters alone: %s", r.Raw)
	}

	// The matching run ended needing attention.
	a.exec(`UPDATE filter_runs SET matched_at = now(), match_status = 'needs_attention', attention_reason = 'too_few_passed' WHERE id = $1`, run.str("id"))
	r := a.want(a.do("GET", "/roles/"+strict+"/match-status", "ops", "", nil), 200, "ops")
	got, _ := r.Body["run"].(map[string]any)
	if r.str("status") != "needs_attention" || got == nil || got["id"] != run.str("id") || got["attention_reason"] != "too_few_passed" {
		t.Fatalf("ops: %s", r.Raw)
	}
	if f, _ := got["top_filter"].(map[string]any); f == nil || f["filter"] != top["filter"] {
		t.Fatalf("ops is not told which must-have eliminated the most: %s", r.Raw)
	}
	r = a.want(a.do("GET", "/roles/"+strict+"/match-status", "employer", "", nil), 200, "employer")
	if r.str("status") != "in_review" || r.Body["run"] != nil {
		t.Fatalf("an employer is told more than \"in review\": %s", r.Raw)
	}
	if list := a.want(a.do("GET", "/matches?role_id="+strict, "employer", "", nil), 200, "employer matches"); len(list.List) != 0 {
		t.Fatalf("an employer sees matches of a role in review: %s", list.Raw)
	}

	// Ops finds two by hand and releases them: ready, for both.
	for _, name := range []string{"Fits", "Tokyo"} {
		id := a.match(strict, p.id(name), 0.9)
		a.want(a.do("POST", "/matches/"+id+"/release", "ops", "ops@example.com", nil), 200, "release")
		if name == "Fits" {
			if r := a.want(a.do("GET", "/roles/"+strict+"/match-status", "employer", "", nil), 200, "one released"); r.str("status") != "in_review" || r.Body["released"] != float64(1) {
				t.Fatalf("one released: %s", r.Raw)
			}
		}
	}
	for _, persona := range []string{"ops", "employer"} {
		if r := a.want(a.do("GET", "/roles/"+strict+"/match-status", persona, "", nil), 200, persona+" two released"); r.str("status") != "ready" || r.Body["released"] != float64(2) {
			t.Fatalf("%s with two released: %s", persona, r.Raw)
		}
	}

	a.want(a.do("GET", "/roles/"+roleID+"/match-status", "talent", "", nil), 403, "talent")
	a.want(a.do("GET", "/roles/00000000-0000-0000-0000-000000000000/match-status", "ops", "", nil), 404, "no such role")
}
