package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func workHours(tz, start, end string, hours int, from string) map[string]any {
	return map[string]any{"timezone": tz, "work_start": start, "work_end": end, "hours_per_week": hours, "available_from": from}
}

func TestCandidateSetsAndEditsAvailability(t *testing.T) {
	a := newAPI(t)
	r := a.want(a.do("POST", "/candidates", "talent", "", map[string]any{"full_name": "Ben Larsen"}), 201, "talent create")
	mine := r.str("id")
	other := a.candidate("Chloe Martin", "chloe@example.com")
	path := "/candidates/" + mine + "/availability"

	a.want(a.do("GET", path, "talent", mine, nil), 404, "nothing supplied yet")

	// Set: a 201 with what was sent.
	r = a.want(a.do("PUT", path, "talent", mine, workHours("America/Chicago", "08:30", "17:00", 40, "2026-11-02")), 201, "set")
	if r.str("timezone") != "America/Chicago" || r.str("work_start") != "08:30" || r.str("work_end") != "17:00" ||
		r.Body["hours_per_week"] != float64(40) || r.str("available_from") != "2026-11-02" || r.str("candidate_id") != mine {
		t.Fatalf("set: %s", r.Raw)
	}
	r = a.want(a.do("GET", path, "talent", mine, nil), 200, "read back")
	if r.str("work_start") != "08:30" || r.str("available_from") != "2026-11-02" {
		t.Fatalf("read back: %s", r.Raw)
	}

	// Edit: a 200 that replaces every field. Hours may run past midnight.
	r = a.want(a.do("PUT", path, "talent", mine, workHours("Asia/Manila", "21:00", "06:00", 25, "2026-12-01")), 200, "edit")
	if r.str("timezone") != "Asia/Manila" || r.str("work_end") != "06:00" || r.Body["hours_per_week"] != float64(25) {
		t.Fatalf("edit: %s", r.Raw)
	}
	if n := a.count(`SELECT count(*) FROM candidate_availability WHERE candidate_id = $1`, mine); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}

	// All four answers are required, and each is checked.
	r = a.want(a.do("PUT", path, "talent", mine, map[string]any{}), 422, "empty")
	for _, f := range []string{"timezone", "work_start", "work_end", "hours_per_week", "available_from"} {
		if r.field(f) != "required" {
			t.Errorf("missing %s should be reported as required: %s", f, r.Raw)
		}
	}
	r = a.want(a.do("PUT", path, "talent", mine, workHours("Mars/Olympus", "9am", "25:00", 0, "2026-12-01")), 422, "bad values")
	for _, f := range []string{"timezone", "work_start", "work_end", "hours_per_week"} {
		if r.field(f) == "" {
			t.Errorf("expected a field error for %s: %s", f, r.Raw)
		}
	}
	r = a.want(a.do("PUT", path, "talent", mine, workHours("Europe/London", "09:00", "09:00", 81, "2026-12-01")), 422, "no hours")
	if r.field("work_end") == "" || r.field("hours_per_week") == "" {
		t.Fatalf("expected work_end and hours_per_week errors: %s", r.Raw)
	}
	a.want(a.do("PUT", path, "talent", mine, workHours("Europe/London", "09:00", "17:00", 40, "next week")), 400, "bad date")
	a.want(a.do("PUT", path, "talent", mine, `{"time_zone":"Europe/London"}`), 400, "unknown field")
	// A refused edit leaves what was stored.
	if r = a.want(a.do("GET", path, "ops", "", nil), 200, "ops reads"); r.str("timezone") != "Asia/Manila" {
		t.Fatalf("a refused edit changed the row: %s", r.Raw)
	}

	// Scoping: talent only their own, employers not at all, ops anyone's.
	a.want(a.do("GET", "/candidates/"+other+"/availability", "talent", mine, nil), 404, "talent reads other")
	a.want(a.do("PUT", "/candidates/"+other+"/availability", "talent", mine, workHours("Europe/London", "09:00", "17:00", 40, "2026-12-01")), 404, "talent sets other")
	a.want(a.do("GET", path, "employer", "", nil), 403, "employer reads")
	a.want(a.do("PUT", "/candidates/"+other+"/availability", "ops", "", workHours("Europe/London", "09:00", "17:00", 40, "2026-12-01")), 201, "ops sets")
	a.want(a.do("PUT", "/candidates/00000000-0000-0000-0000-000000000000/availability", "ops", "", workHours("Europe/London", "09:00", "17:00", 40, "2026-12-01")), 404, "no such candidate")

	// Replacing the profile, as a resume parse does, leaves the answers alone.
	a.want(a.do("PUT", "/candidates/"+mine+"/profile", "ops", "", map[string]any{"timezone": "America/Denver", "availability": "immediate"}), 201, "profile")
	if r = a.want(a.do("GET", path, "talent", mine, nil), 200, "after profile"); r.str("timezone") != "Asia/Manila" {
		t.Fatalf("a profile write changed the availability: %s", r.Raw)
	}

	// It goes with the candidate.
	a.want(a.do("DELETE", "/candidates/"+mine, "ops", "", nil), 204, "delete candidate")
	if n := a.count(`SELECT count(*) FROM candidate_availability WHERE candidate_id = $1`, mine); n != 0 {
		t.Fatalf("availability outlived its candidate")
	}
}

func TestRoleRecordsTheOverlapItRequires(t *testing.T) {
	a := newAPI(t)
	r := a.want(a.do("POST", "/roles", "employer", "", map[string]any{
		"title": "Bookkeeper", "timezone": "America/Chicago", "min_overlap_hours": 4, "hours_per_week": 20,
	}), 201, "create")
	id := r.str("id")
	if r.Body["min_overlap_hours"] != float64(4) || r.Body["hours_per_week"] != float64(20) {
		t.Fatalf("create: %s", r.Raw)
	}
	// An edit that does not mention them keeps them; 0 and null both clear.
	r = a.want(a.do("PUT", "/roles/"+id, "employer", "", map[string]any{"company": "Acme"}), 200, "unrelated edit")
	if r.Body["min_overlap_hours"] != float64(4) || r.Body["hours_per_week"] != float64(20) {
		t.Fatalf("unrelated edit dropped them: %s", r.Raw)
	}
	r = a.want(a.do("PUT", "/roles/"+id, "employer", "", map[string]any{"min_overlap_hours": 0, "hours_per_week": nil}), 200, "clear")
	if r.Body["min_overlap_hours"] != nil || r.Body["hours_per_week"] != nil {
		t.Fatalf("clear: %s", r.Raw)
	}
	// A role created without them (the JD did not say) has them supplied later.
	bare := a.role("Controller")
	if r = a.want(a.do("GET", "/roles/"+bare, "ops", "", nil), 200, "bare"); r.Body["min_overlap_hours"] != nil || r.Body["hours_per_week"] != nil {
		t.Fatalf("unstated requirements should be null: %s", r.Raw)
	}
	r = a.want(a.do("PUT", "/roles/"+bare, "employer", "", map[string]any{"min_overlap_hours": 3}), 422, "overlap without a zone")
	if r.field("timezone") == "" {
		t.Fatalf("expected a timezone error: %s", r.Raw)
	}
	a.want(a.do("PUT", "/roles/"+bare, "employer", "", map[string]any{"min_overlap_hours": 3, "timezone": "Europe/London", "hours_per_week": 35}), 200, "supplied at intake")
	r = a.want(a.do("PUT", "/roles/"+bare, "employer", "", map[string]any{"timezone": nil}), 422, "zone removed from under the overlap")
	if r.field("timezone") == "" {
		t.Fatalf("expected a timezone error: %s", r.Raw)
	}
	r = a.want(a.do("POST", "/roles", "employer", "", map[string]any{"title": "x", "timezone": "Europe/London", "min_overlap_hours": 9, "hours_per_week": 81}), 422, "out of range")
	if r.field("min_overlap_hours") == "" || r.field("hours_per_week") == "" {
		t.Fatalf("expected range errors: %s", r.Raw)
	}
}

func TestCandidateWithoutAvailabilityIsExcludedFromMatching(t *testing.T) {
	a := newAPI(t)
	silent := a.candidate("Ada Okafor", "ada@example.com")     // never answers
	fits := a.candidate("Ben Larsen", "ben@example.com")       // meets everything
	far := a.candidate("Chloe Martin", "chloe@example.com")    // wrong side of the world, part time, late
	archived := a.candidate("Dan Archived", "dan@example.com") // answered, but not active
	a.want(a.do("PUT", "/candidates/"+fits+"/availability", "ops", "", workHours("America/New_York", "09:00", "17:00", 40, "2020-01-01")), 201, "fits")
	a.want(a.do("PUT", "/candidates/"+far+"/availability", "ops", "", workHours("Asia/Tokyo", "09:00", "17:00", 15, "2999-01-01")), 201, "far")
	a.want(a.do("PUT", "/candidates/"+archived+"/availability", "ops", "", workHours("America/Chicago", "09:00", "17:00", 40, "2020-01-01")), 201, "archived")
	a.want(a.do("PUT", "/candidates/"+archived, "ops", "", map[string]any{"status": "archived"}), 200, "archive")
	// A parsed resume suggesting a zone and a date is not the candidate's answer.
	a.want(a.do("PUT", "/candidates/"+silent+"/profile", "ops", "", map[string]any{"timezone": "America/Chicago", "availability": "immediate", "available_from": "2020-01-01"}), 201, "profile")

	check := func(roleID string) map[string]map[string]any {
		t.Helper()
		r := a.want(a.do("GET", "/roles/"+roleID+"/availability", "ops", "", nil), 200, "filter")
		out := map[string]map[string]any{}
		for _, row := range r.List {
			out[row["candidate_id"].(string)] = row
		}
		if len(out) != 3 || out[archived] != nil {
			t.Fatalf("want the three active candidates: %s", r.Raw)
		}
		if r.List[0]["candidate_name"] != "Ada Okafor" {
			t.Fatalf("want candidates by name: %s", r.Raw)
		}
		return out
	}

	// A role that asks for nothing still excludes the candidate who never
	// answered, and says why.
	open := check(a.role("Anything Goes"))
	if open[silent]["passed"] != false || !strings.Contains(fmt.Sprint(open[silent]["reasons"]), "Has not given") {
		t.Fatalf("no availability must not pass: %v", open[silent])
	}
	if open[fits]["passed"] != true || open[far]["passed"] != true || fmt.Sprint(open[fits]["reasons"]) != "[]" {
		t.Fatalf("a role with no requirements passes everyone who answered: %v", open)
	}

	// A role with requirements.
	r := a.want(a.do("POST", "/roles", "employer", "", map[string]any{
		"title": "Senior Accountant", "timezone": "America/Chicago", "min_overlap_hours": 4, "hours_per_week": 40, "starts_on": "2030-01-01",
	}), 201, "role")
	strict := check(r.str("id"))
	if strict[fits]["passed"] != true || strict[fits]["overlap_hours"] != float64(7) {
		t.Fatalf("New York covers seven hours of a Chicago day: %v", strict[fits])
	}
	if strict[silent]["passed"] != false || strict[silent]["overlap_hours"] != nil {
		t.Fatalf("no availability must not pass: %v", strict[silent])
	}
	reasons := fmt.Sprint(strict[far]["reasons"])
	for _, want := range []string{"Available from 2999-01-01", "Offers 15 hours a week", "this role needs 4"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("reasons should include %q: %s", want, reasons)
		}
	}
	if strict[far]["passed"] != false {
		t.Fatalf("far should fail: %v", strict[far])
	}

	// Nobody can be matched without answers, even by hand; with them, ops decides.
	m := a.want(a.do("POST", "/matches", "ops", "", map[string]any{"role_id": r.str("id"), "candidate_id": silent, "score": 0.9}), 422, "match without availability")
	if !strings.Contains(m.field("candidate_id"), "cannot be matched") {
		t.Fatalf("expected the reason on candidate_id: %s", m.Raw)
	}
	if n := a.count(`SELECT count(*) FROM matches`); n != 0 {
		t.Fatalf("a match was stored for a candidate without availability")
	}
	a.want(a.do("POST", "/matches", "ops", "", map[string]any{"role_id": r.str("id"), "candidate_id": fits, "score": 0.9}), 201, "match with availability")

	a.want(a.do("GET", "/roles/00000000-0000-0000-0000-000000000000/availability", "ops", "", nil), 404, "no such role")
	a.want(a.do("GET", "/roles/"+r.str("id")+"/availability", "employer", "", nil), 403, "employer")
	a.want(a.do("GET", "/roles/"+r.str("id")+"/availability", "talent", fits, nil), 403, "talent")
}

// The table constraints hold whatever writes to them, not only the API.
func TestAvailabilityConstraints(t *testing.T) {
	a := newAPI(t)
	id := a.candidate("Ada Okafor", "ada@example.com")
	role := a.role("Bookkeeper")
	for what, sql := range map[string]string{
		"no working hours":       `INSERT INTO candidate_availability VALUES ('` + id + `', 'UTC', '09:00', '09:00', 40, '2026-01-01')`,
		"no hours a week":        `INSERT INTO candidate_availability VALUES ('` + id + `', 'UTC', '09:00', '17:00', 0, '2026-01-01')`,
		"a partial answer":       `INSERT INTO candidate_availability (candidate_id, timezone) VALUES ('` + id + `', 'UTC')`,
		"overlap without a zone": `UPDATE roles SET min_overlap_hours = 4 WHERE id = '` + role + `'`,
		"overlap out of range":   `UPDATE roles SET timezone = 'UTC', min_overlap_hours = 9 WHERE id = '` + role + `'`,
	} {
		if _, err := a.pool.Exec(context.Background(), sql); err == nil {
			t.Errorf("%s: the database accepted it", what)
		}
	}
}
