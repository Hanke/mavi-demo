package server

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/availability"
)

// filterPool is a pool in which every candidate but Fits misses exactly one
// thing, so each filter can be seen to drop its own candidates and no others.
type filterPool struct {
	*api
	names map[string]string // candidate id -> name
}

// The candidate everyone else is a variation of: a US CPA on NetSuite and
// Excel with eight years, working New York office hours, full time, free now.
func fitsProfile() map[string]any {
	return map[string]any{"certifications": []string{"cpa_us"}, "software": []string{"netsuite", "excel"}, "years_experience": 8}
}

func fitsHours() map[string]any {
	return workHours("America/New_York", "09:00", "17:00", 40, "2020-01-01")
}

// with is base with some fields replaced; a nil value removes the field.
func with(base map[string]any, changes map[string]any) map[string]any {
	for k, v := range changes {
		if v == nil {
			delete(base, k)
		} else {
			base[k] = v
		}
	}
	return base
}

// add creates a candidate; a nil profile or hours is one they do not have.
func (p *filterPool) add(name string, profile, hours map[string]any) string {
	p.t.Helper()
	id := p.candidate(name, strings.ToLower(strings.ReplaceAll(name, " ", "."))+"@example.com")
	if profile != nil {
		p.want(p.do("PUT", "/candidates/"+id+"/profile", "ops", "", profile), 201, name+": profile")
	}
	if hours != nil {
		p.want(p.do("PUT", "/candidates/"+id+"/availability", "ops", "", hours), 201, name+": availability")
	}
	p.names[id] = name
	return id
}

func newFilterPool(t *testing.T) *filterPool {
	p := &filterPool{api: newAPI(t), names: map[string]string{}}
	p.add("Fits", fitsProfile(), fitsHours())
	p.add("No Profile", nil, fitsHours())
	p.add("No Answers", fitsProfile(), nil)
	p.add("ACCA", with(fitsProfile(), map[string]any{"certifications": []string{"acca"}}), fitsHours())
	p.add("Unplaced CPA", with(fitsProfile(), map[string]any{"certifications": []string{"cpa"}}), fitsHours())
	p.add("No Certificate", with(fitsProfile(), map[string]any{"certifications": []string{}}), fitsHours())
	p.add("Excel Only", with(fitsProfile(), map[string]any{"software": []string{"excel"}}), fitsHours())
	p.add("Two Years", with(fitsProfile(), map[string]any{"years_experience": 2}), fitsHours())
	p.add("Years Unknown", with(fitsProfile(), map[string]any{"years_experience": nil}), fitsHours())
	p.add("Starts Late", fitsProfile(), with(fitsHours(), map[string]any{"available_from": "2999-01-01"}))
	p.add("Part Time", fitsProfile(), with(fitsHours(), map[string]any{"hours_per_week": 15}))
	p.add("Tokyo", fitsProfile(), with(fitsHours(), map[string]any{"timezone": "Asia/Tokyo"}))
	archived := p.add("Archived", fitsProfile(), fitsHours())
	p.want(p.do("PUT", "/candidates/"+archived, "ops", "", map[string]any{"status": "archived"}), 200, "archive")
	return p
}

const filterPoolSize = 12 // the active candidates of newFilterPool

// filterResult is a run as the tests read it.
type filterResult struct {
	raw      string
	pool     int
	passed   []string       // names, in the order returned
	excluded map[string]int // filter -> candidates it dropped
	order    []string       // filters, in the order returned
}

// run creates a role with the given requirements and runs its hard filters.
func (p *filterPool) run(requirements map[string]any) filterResult {
	p.t.Helper()
	role := p.want(p.do("POST", "/roles", "employer", "", with(map[string]any{"title": "Role"}, requirements)), 201, "role")
	return p.runRole(role.str("id"))
}

func (p *filterPool) runRole(roleID string) filterResult {
	p.t.Helper()
	r := p.want(p.do("POST", "/roles/"+roleID+"/filter-runs", "ops", "", nil), 201, "run filters")
	out := filterResult{raw: r.Raw, pool: int(r.Body["pool"].(float64)), excluded: map[string]int{}}
	for _, id := range r.Body["candidate_ids"].([]any) {
		out.passed = append(out.passed, p.names[id.(string)])
	}
	left := out.pool
	for _, s := range r.Body["stages"].([]any) {
		stage := s.(map[string]any)
		name, remaining, excluded := stage["filter"].(string), int(stage["remaining"].(float64)), int(stage["excluded"].(float64))
		if remaining+excluded != left {
			p.t.Fatalf("%s: remaining %d + excluded %d != the %d who reached it: %s", name, remaining, excluded, left, r.Raw)
		}
		left = remaining
		out.order = append(out.order, name)
		out.excluded[name] = excluded
	}
	if int(r.Body["passed"].(float64)) != left || len(out.passed) != left {
		p.t.Fatalf("passed should be the %d left after the last filter and the length of candidate_ids: %s", left, r.Raw)
	}
	return out
}

// dropped fails the test unless the run dropped exactly these candidates
// beyond the two no role can pass (no profile, no answers), all at filter.
func (res filterResult) dropped(t *testing.T, filter string, names ...string) {
	t.Helper()
	want := map[string]int{"profile": 1, "availability": 1}
	want[filter] += len(names)
	for _, f := range res.order {
		if res.excluded[f] != want[f] {
			t.Errorf("%s dropped %d, want %d: %s", f, res.excluded[f], want[f], res.raw)
		}
	}
	for _, name := range append([]string{"No Profile", "No Answers", "Archived"}, names...) {
		if slices.Contains(res.passed, name) {
			t.Errorf("%s must not pass: %v", name, res.passed)
		}
	}
	if len(res.passed) != filterPoolSize-2-len(names) {
		t.Errorf("passed = %v, want everyone but No Profile, No Answers and %v", res.passed, names)
	}
}

func TestHardFilters(t *testing.T) {
	p := newFilterPool(t)

	t.Run("a role that asks for nothing", func(t *testing.T) {
		res := p.run(nil)
		if res.pool != filterPoolSize {
			t.Fatalf("pool = %d, want the %d active candidates: %s", res.pool, filterPoolSize, res.raw)
		}
		want := []string{"profile", "certifications", "software", "experience", "availability", "timezone_overlap"}
		if !slices.Equal(res.order, want) {
			t.Fatalf("filters = %v, want %v", res.order, want)
		}
		// Even so, a candidate with no profile or no answers is not passed on.
		res.dropped(t, "profile")
		if !slices.IsSorted(res.passed) {
			t.Errorf("passed should be by name: %v", res.passed)
		}
	})

	t.Run("certifications", func(t *testing.T) {
		// A bare id accepts equivalents: the ACCA passes, the CPA from nobody
		// knows where does not.
		p.run(map[string]any{"required_certifications": []string{"cpa_us"}}).
			dropped(t, "certifications", "Unplaced CPA", "No Certificate")
		// The parser's record says the qualification itself is required.
		p.run(map[string]any{
			"required_certifications": []string{"cpa_us"},
			"requirements": map[string]any{"required_qualifications": []map[string]any{
				{"name_as_written": "active US CPA licence", "canonical": "cpa_us", "accept_equivalents": false},
			}},
		}).dropped(t, "certifications", "ACCA", "Unplaced CPA", "No Certificate")
		// "A CPA" is any CPA; and it takes its equivalents too.
		p.run(map[string]any{"required_certifications": []string{"cpa"}}).dropped(t, "certifications", "No Certificate")
		// Every requirement must be met, not any: nobody here is also a CMA.
		res := p.run(map[string]any{"required_certifications": []string{"cpa_us", "cma"}})
		if len(res.passed) != 0 || res.excluded["certifications"] != filterPoolSize-1 {
			t.Errorf("nobody holds both: %s", res.raw)
		}
	})

	t.Run("software", func(t *testing.T) {
		p.run(map[string]any{"required_software": []string{"NetSuite", "excel"}}).dropped(t, "software", "Excel Only")
		p.run(map[string]any{"required_software": []string{"excel"}}).dropped(t, "software")
	})

	t.Run("experience", func(t *testing.T) {
		// A profile that does not say how many years does not pass a minimum.
		p.run(map[string]any{"min_years_experience": 5}).dropped(t, "experience", "Two Years", "Years Unknown")
		p.run(map[string]any{"min_years_experience": 2}).dropped(t, "experience", "Years Unknown")
	})

	t.Run("availability", func(t *testing.T) {
		p.run(map[string]any{"starts_on": "2030-01-01"}).dropped(t, "availability", "Starts Late")
		p.run(map[string]any{"hours_per_week": 40}).dropped(t, "availability", "Part Time")
		p.run(map[string]any{"hours_per_week": 15, "starts_on": "2999-01-01"}).dropped(t, "availability")
	})

	t.Run("timezone overlap", func(t *testing.T) {
		// Tokyo's office hours miss a Chicago day entirely; New York's cover seven hours of it.
		p.run(map[string]any{"timezone": "America/Chicago", "min_overlap_hours": 4}).dropped(t, "timezone_overlap", "Tokyo")
		p.run(map[string]any{"timezone": "America/Chicago", "min_overlap_hours": 8}).
			dropped(t, "timezone_overlap", "Tokyo", "Fits", "ACCA", "Unplaced CPA", "No Certificate", "Excel Only", "Two Years", "Years Unknown", "Starts Late", "Part Time")
		// A zone with no minimum asks for no overlap.
		p.run(map[string]any{"timezone": "America/Chicago"}).dropped(t, "timezone_overlap")
	})

	t.Run("every must-have at once", func(t *testing.T) {
		role := p.want(p.do("POST", "/roles", "employer", "", map[string]any{
			"title": "Senior Accountant", "required_certifications": []string{"cpa_us"}, "required_software": []string{"netsuite"},
			"min_years_experience": 5, "starts_on": "2030-01-01", "hours_per_week": 40,
			"timezone": "America/Chicago", "min_overlap_hours": 4,
		}), 201, "role").str("id")
		res := p.runRole(role)
		if !slices.Equal(res.passed, []string{"ACCA", "Fits"}) {
			t.Fatalf("passed = %v, want ACCA and Fits: %s", res.passed, res.raw)
		}
		want := map[string]int{"profile": 1, "certifications": 2, "software": 1, "experience": 2, "availability": 3, "timezone_overlap": 1}
		for f, n := range want {
			if res.excluded[f] != n {
				t.Errorf("%s dropped %d, want %d: %s", f, res.excluded[f], n, res.raw)
			}
		}

		// The run is recorded, newest first, as it was returned.
		again := p.runRole(role)
		list := p.want(p.do("GET", "/roles/"+role+"/filter-runs", "ops", "", nil), 200, "list runs")
		if len(list.List) != 2 || fmt.Sprint(list.List[0]["stages"]) != fmt.Sprint(list.List[1]["stages"]) ||
			list.List[0]["overlap_on"] != "2030-01-01" || list.List[0]["role_id"] != role {
			t.Fatalf("want both runs, identical, worked out for the start date: %s", list.Raw)
		}
		if !strings.Contains(again.raw, list.List[0]["id"].(string)) {
			t.Fatalf("want the newest run first: %s", list.Raw)
		}
		if n := p.count(`SELECT count(*) FROM filter_runs WHERE role_id = $1 AND pool = $2 AND after_timezone_overlap = 2`, role, filterPoolSize); n != 2 {
			t.Fatalf("filter_runs rows = %d, want 2", n)
		}
		// A candidate who fails several filters is counted once, at the first.
		p.add("Fails Everything", map[string]any{}, workHours("Asia/Tokyo", "09:00", "17:00", 5, "2999-01-01"))
		if res := p.runRole(role); res.pool != filterPoolSize+1 || res.excluded["certifications"] != 3 || res.excluded["availability"] != 3 {
			t.Fatalf("want one more dropped, at certifications only: %s", res.raw)
		}
		// The record goes with the role.
		p.want(p.do("DELETE", "/roles/"+role, "ops", "", nil), 204, "delete role")
		if n := p.count(`SELECT count(*) FROM filter_runs WHERE role_id = $1`, role); n != 0 {
			t.Fatalf("filter runs outlived their role")
		}
	})

	t.Run("who may run it", func(t *testing.T) {
		role := p.role("Bookkeeper")
		for _, method := range []string{"POST", "GET"} {
			p.want(p.do(method, "/roles/"+role+"/filter-runs", "employer", "", nil), 403, "employer")
			p.want(p.do(method, "/roles/"+role+"/filter-runs", "talent", "x", nil), 403, "talent")
			p.want(p.do(method, "/roles/00000000-0000-0000-0000-000000000000/filter-runs", "ops", "", nil), 404, "no such role")
			p.want(p.do(method, "/roles/not-a-uuid/filter-runs", "ops", "", nil), 404, "not an id")
		}
		if r := p.want(p.do("GET", "/roles/"+role+"/filter-runs", "ops", "", nil), 200, "no runs yet"); r.Raw == "" || len(r.List) != 0 {
			t.Fatalf("want an empty list: %s", r.Raw)
		}
	})
}

func TestHardFiltersOnAnEmptyPool(t *testing.T) {
	a := newAPI(t)
	r := a.want(a.do("POST", "/roles/"+a.role("Controller")+"/filter-runs", "ops", "", nil), 201, "run")
	if r.Body["pool"] != float64(0) || r.Body["passed"] != float64(0) || fmt.Sprint(r.Body["candidate_ids"]) != "[]" {
		t.Fatalf("want an empty run: %s", r.Raw)
	}
}

// The counts reach the log as well as the table.
func TestHardFilterLogsTheFunnel(t *testing.T) {
	p := newFilterPool(t)
	role := p.role("Controller")
	var out bytes.Buffer
	log.SetOutput(&out)
	defer log.SetOutput(os.Stderr)
	p.runRole(role)
	want := `("Controller"): pool 12 -> profile 11 -> certifications 11 -> software 11 -> experience 11 -> availability 10 -> timezone_overlap 10`
	if got := out.String(); strings.Count(got, "hard filter: ") != 1 || !strings.Contains(got, want) || !strings.Contains(got, role) {
		t.Fatalf("log = %q, want one line for the role with %q", got, want)
	}
}

// overlap_minutes is the SQL the filter runs; availability.OverlapMinutes is
// the Go the per-candidate listing runs. They must agree, including on the
// days a zone changes its clocks.
func TestOverlapMinutesInSQL(t *testing.T) {
	a := newAPI(t)
	sql := func(role, cand, start, end, on string) *int {
		t.Helper()
		var minutes *int
		err := a.pool.QueryRow(context.Background(), `SELECT overlap_minutes($1, $2, $3::time, $4::time, $5::date)`, role, cand, start, end, on).Scan(&minutes)
		if err != nil {
			t.Fatal(err)
		}
		return minutes
	}

	cases := []struct {
		name                       string
		role, cand, start, end, on string
		want                       int
	}{
		{"same zone, same hours", "America/Chicago", "America/Chicago", "09:00", "17:00", "2026-01-14", 480},
		{"a longer day counts only the role's", "America/Chicago", "America/Chicago", "07:00", "20:00", "2026-01-14", 480},
		{"three zones west", "America/New_York", "America/Los_Angeles", "09:00", "17:00", "2026-01-14", 300},
		{"London afternoon meets New York morning", "America/New_York", "Europe/London", "09:00", "17:00", "2026-01-14", 180},
		{"no overlap across the Pacific", "America/New_York", "Asia/Manila", "09:00", "17:00", "2026-01-14", 0},
		{"a night shift past midnight covers it", "America/New_York", "Asia/Manila", "21:00", "06:00", "2026-01-14", 480},
		{"a shift that began the day before", "Europe/London", "Australia/Sydney", "18:00", "02:00", "2026-01-14", 360},
		{"half-hour zone", "Europe/London", "Asia/Kolkata", "09:00", "17:00", "2026-01-14", 150},
		{"an end at midnight", "America/Chicago", "America/Chicago", "13:00", "00:00", "2026-01-14", 240},
		{"hours to the minute", "America/Chicago", "America/Chicago", "09:15", "11:50", "2026-01-14", 155},
		// The US is on summer time from 8 March 2026, the UK from 29 March.
		{"between the two clock changes", "America/New_York", "Europe/London", "09:00", "17:00", "2026-03-16", 240},
		{"after both", "America/New_York", "Europe/London", "09:00", "17:00", "2026-04-01", 180},
		// New York's clocks go back on 1 November 2026: 01:00 to 02:00 happens twice.
		{"an hour that happens twice", "Asia/Tokyo", "America/New_York", "01:00", "02:00", "2026-11-01", 120},
		// And forward on 8 March: 02:00 to 03:00 does not happen.
		{"an hour that does not happen", "Asia/Tokyo", "America/New_York", "02:00", "03:00", "2026-03-08", 0},
	}
	for _, c := range cases {
		got := sql(c.role, c.cand, c.start, c.end, c.on)
		if got == nil || *got != c.want {
			t.Errorf("%s: overlap_minutes = %v, want %d", c.name, deref(got), c.want)
		}
		roleLoc, _ := time.LoadLocation(c.role)
		candLoc, _ := time.LoadLocation(c.cand)
		start, _ := availability.ParseClock(c.start)
		end, _ := availability.ParseClock(c.end)
		on, _ := time.Parse(time.DateOnly, c.on)
		if inGo := availability.OverlapMinutes(roleLoc, candLoc, start, end, on); inGo != c.want {
			t.Errorf("%s: SQL and Go disagree: availability.OverlapMinutes = %d", c.name, inGo)
		}
	}

	// A zone Postgres cannot read is no answer, not an overlap of zero.
	if got := sql("America/Chicago", "Mars/Olympus", "09:00", "17:00", "2026-01-14"); got != nil {
		t.Errorf("unknown candidate zone: overlap_minutes = %d, want NULL", *got)
	}
	if got := sql("Mars/Olympus", "America/Chicago", "09:00", "17:00", "2026-01-14"); got != nil {
		t.Errorf("unknown role zone: overlap_minutes = %d, want NULL", *got)
	}
}

// A candidate whose stored zone is not an IANA name fails a role that has a
// zone, even one asking for no overlap, as availability.Check has it. That
// includes the strings Postgres would read as a zone and Go does not.
func TestHardFilterUnreadableZoneDoesNotPass(t *testing.T) {
	p := &filterPool{api: newAPI(t), names: map[string]string{}}
	p.add("Fits", fitsProfile(), fitsHours())
	lost := p.add("Lost", fitsProfile(), fitsHours())
	for _, zone := range []string{"Mars/Olympus", "XYZ5", "UTC+5", "america/new_york", ""} {
		p.exec(`UPDATE candidate_availability SET timezone = $2 WHERE candidate_id = $1`, lost, zone)
		res := p.run(map[string]any{"timezone": "America/Chicago"})
		if !slices.Equal(res.passed, []string{"Fits"}) || res.excluded["timezone_overlap"] != 1 {
			t.Errorf("candidate zone %q must not pass: %s", zone, res.raw)
		}
	}
	if res := p.run(nil); len(res.passed) != 2 {
		t.Fatalf("a role with no zone does not look at the candidate's: %s", res.raw)
	}

	// The same for the role's own zone, written past the API: nobody passes.
	role := p.role("Bookkeeper")
	for _, zone := range []string{"Mars/Olympus", "XYZ5"} {
		p.exec(`UPDATE roles SET timezone = $2 WHERE id = $1`, role, zone)
		if res := p.runRole(role); len(res.passed) != 0 || res.excluded["timezone_overlap"] != res.pool {
			t.Errorf("role zone %q: nobody can pass: %s", zone, res.raw)
		}
	}
}

// A minimum of 0 written past the API is no minimum, as NULL is.
func TestHardFilterZeroYearsIsNoMinimum(t *testing.T) {
	p := &filterPool{api: newAPI(t), names: map[string]string{}}
	p.add("Years Unknown", with(fitsProfile(), map[string]any{"years_experience": nil}), fitsHours())
	role := p.role("Bookkeeper")
	p.exec(`UPDATE roles SET min_years_experience = 0 WHERE id = $1`, role)
	if res := p.runRole(role); len(res.passed) != 1 {
		t.Fatalf("a minimum of 0 dropped a profile with no figure: %s", res.raw)
	}
}

func deref(p *int) any {
	if p == nil {
		return "NULL"
	}
	return *p
}
