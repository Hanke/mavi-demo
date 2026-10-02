package availability

import (
	"strings"
	"testing"
	"time"
)

func loc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func date(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

func ptr[T any](v T) *T { return &v }

func TestParseClock(t *testing.T) {
	for in, want := range map[string]int{"00:00": 0, "09:00": 540, "17:30": 1050, "23:59": 1439} {
		if got, ok := ParseClock(in); !ok || got != want {
			t.Errorf("ParseClock(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "9:00", "24:00", "12:60", "12-00", "ab:cd", "09:00:00"} {
		if _, ok := ParseClock(in); ok {
			t.Errorf("ParseClock(%q) should fail", in)
		}
	}
}

func TestOverlapMinutes(t *testing.T) {
	winter := date("2026-01-14") // no zone below is on summer time
	cases := []struct {
		name       string
		role, cand string
		start, end int
		on         time.Time
		want       int
	}{
		{"same zone, same hours", "America/Chicago", "America/Chicago", 9 * 60, 17 * 60, winter, 480},
		{"same zone, longer day counts only the role's", "America/Chicago", "America/Chicago", 7 * 60, 20 * 60, winter, 480},
		{"three zones west", "America/New_York", "America/Los_Angeles", 9 * 60, 17 * 60, winter, 300},
		{"London afternoon meets New York morning", "America/New_York", "Europe/London", 9 * 60, 17 * 60, winter, 180},
		{"no overlap across the Pacific", "America/New_York", "Asia/Manila", 9 * 60, 17 * 60, winter, 0},
		{"a night shift past midnight covers it", "America/New_York", "Asia/Manila", 21 * 60, 6 * 60, winter, 480},
		{"a shift that began the day before", "Europe/London", "Australia/Sydney", 18 * 60, 2 * 60, winter, 360},
		{"half-hour zone", "Europe/London", "Asia/Kolkata", 9 * 60, 17 * 60, winter, 150},
		{"part-time mornings", "America/Chicago", "America/Chicago", 8 * 60, 12 * 60, winter, 180},
		// The US is on summer time from 8 March 2026, the UK from 29 March:
		// for three weeks London is four hours ahead of New York, not five.
		{"clocks changed in one zone only", "America/New_York", "Europe/London", 9 * 60, 17 * 60, date("2026-03-16"), 240},
		// The candidate's clocks change during the role's day. Sydney goes
		// back at 03:00 on 5 April 2026, so 02:00 to 03:00 happens twice.
		{"an hour the candidate's clock repeats", "America/New_York", "Australia/Sydney", 2 * 60, 3 * 60, date("2026-04-04"), 120},
		// An all-but-one-minute shift can never cover more than the role's day.
		{"a shift that wraps onto itself", "America/New_York", "Australia/Lord_Howe", 2*60 + 30, 2*60 + 29, date("2026-10-03"), 480},
	}
	for _, c := range cases {
		got := OverlapMinutes(loc(t, c.role), loc(t, c.cand), c.start, c.end, c.on)
		if got != c.want {
			t.Errorf("%s: overlap = %d minutes, want %d", c.name, got, c.want)
		}
	}
}

func TestCheckExcludesACandidateWithoutAnswers(t *testing.T) {
	// Even a role that asks for nothing does not pass a candidate who has not
	// said when and where they can work.
	for _, role := range []Role{{}, {Timezone: ptr("America/Chicago"), MinOverlapHours: ptr(4), HoursPerWeek: ptr(20)}} {
		v := Check(role, nil, date("2026-01-14"))
		if v.Passed || len(v.Reasons) != 1 || !strings.Contains(v.Reasons[0], "Has not given") {
			t.Errorf("no answers must fail with the reason, got %+v", v)
		}
		if v.OverlapMinutes != nil {
			t.Errorf("no answers: overlap should be nil, got %d", *v.OverlapMinutes)
		}
	}
}

func TestCheck(t *testing.T) {
	on := date("2026-01-14")
	cand := Candidate{Timezone: "Europe/London", WorkStart: 9 * 60, WorkEnd: 17 * 60, HoursPerWeek: 30, AvailableFrom: date("2026-02-01")}

	// A role with no requirements passes anyone who answered.
	if v := Check(Role{}, &cand, on); !v.Passed || len(v.Reasons) != 0 || v.OverlapMinutes != nil {
		t.Fatalf("no requirements: %+v", v)
	}

	role := Role{Timezone: ptr("America/New_York"), MinOverlapHours: ptr(3), HoursPerWeek: ptr(30), StartsOn: ptr(date("2026-02-01"))}
	v := Check(role, &cand, on)
	if !v.Passed || v.OverlapMinutes == nil || *v.OverlapMinutes != 180 {
		t.Fatalf("meets every requirement exactly: %+v", v)
	}

	// Each requirement fails on its own, and all three are reported together.
	role = Role{Timezone: ptr("America/New_York"), MinOverlapHours: ptr(4), HoursPerWeek: ptr(40), StartsOn: ptr(date("2026-01-31"))}
	v = Check(role, &cand, on)
	want := []string{
		"Available from 2026-02-01; this role starts on 2026-01-31",
		"Offers 30 hours a week; this role needs 40",
		"Working hours overlap the role's day (09:00 to 17:00 America/New_York) by 3 hours; this role needs 4",
	}
	if v.Passed || strings.Join(v.Reasons, "|") != strings.Join(want, "|") {
		t.Fatalf("reasons = %q, want %q", v.Reasons, want)
	}

	// A time zone alone is recorded but filters nothing.
	v = Check(Role{Timezone: ptr("Asia/Tokyo")}, &cand, on)
	if !v.Passed || v.OverlapMinutes == nil || *v.OverlapMinutes != 0 {
		t.Fatalf("zone without a minimum: %+v", v)
	}

	// An overlap with no zone to work it out in cannot be met.
	if v := Check(Role{MinOverlapHours: ptr(4)}, &cand, on); v.Passed || !strings.Contains(v.Reasons[0], "no time zone") {
		t.Fatalf("overlap without a zone passed: %+v", v)
	}

	// A zone that cannot be read fails rather than passing unchecked.
	bad := cand
	bad.Timezone = "Mars/Olympus"
	if v := Check(Role{Timezone: ptr("America/New_York")}, &bad, on); v.Passed {
		t.Fatalf("unreadable zone passed: %+v", v)
	}
}

func TestHours(t *testing.T) {
	for minutes, want := range map[int]string{0: "0 hours", 60: "1 hour", 180: "3 hours", 150: "2.5 hours", 45: "0.75 hours"} {
		if got := hours(minutes); got != want {
			t.Errorf("hours(%d) = %q, want %q", minutes, got, want)
		}
	}
}
