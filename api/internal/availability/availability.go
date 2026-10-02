// Package availability is the hard filter on what a resume cannot say: when
// a candidate can start, how many hours a week they offer, and how much of
// the role's working day their own working hours cover.
//
// The candidate supplies those answers (candidate_availability); a role
// states what it needs (roles.starts_on, hours_per_week, min_overlap_hours,
// timezone). A candidate who has supplied nothing fails every role, including
// one that asks for nothing: missing answers exclude, they never pass.
package availability

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

// A role's working day, local to its time zone. A role states how much of
// this day it needs covered, not the day itself.
const (
	RoleDayStart = 9 * 60
	RoleDayEnd   = 17 * 60
)

// Candidate is what the candidate said about when and where they can work.
type Candidate struct {
	Timezone      string // IANA name
	WorkStart     int    // minutes after local midnight
	WorkEnd       int    // minutes after local midnight; at or before WorkStart runs past midnight
	HoursPerWeek  int
	AvailableFrom time.Time // a calendar day
}

// Role is what the role requires. A nil field is a requirement the role
// does not make.
type Role struct {
	Timezone        *string // IANA name
	MinOverlapHours *int
	HoursPerWeek    *int
	StartsOn        *time.Time // a calendar day
}

// Verdict is the outcome for one candidate against one role.
type Verdict struct {
	Passed  bool
	Reasons []string // why not, in words; empty when Passed
	// OverlapMinutes is how much of the role's working day the candidate
	// covers; nil when the role has no time zone or the candidate no answers.
	OverlapMinutes *int
}

// ParseClock reads "HH:MM" on a 24-hour clock as minutes after midnight.
func ParseClock(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	h, errH := strconv.Atoi(s[:2])
	m, errM := strconv.Atoi(s[3:])
	if errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// Check applies the role's requirements to the candidate's answers. c is nil
// for a candidate who has not supplied them. on names the calendar day the
// overlap is worked out for (its own year, month and day, whatever its zone),
// which matters when the two zones change their clocks on different dates.
func Check(role Role, c *Candidate, on time.Time) Verdict {
	if c == nil {
		return Verdict{Reasons: []string{"Has not given their time zone, working hours, hours per week and start date"}}
	}
	var v Verdict
	if role.StartsOn != nil && day(c.AvailableFrom).After(day(*role.StartsOn)) {
		v.Reasons = append(v.Reasons, fmt.Sprintf("Available from %s; this role starts on %s",
			c.AvailableFrom.Format(time.DateOnly), role.StartsOn.Format(time.DateOnly)))
	}
	if role.HoursPerWeek != nil && c.HoursPerWeek < *role.HoursPerWeek {
		v.Reasons = append(v.Reasons, fmt.Sprintf("Offers %d hours a week; this role needs %d", c.HoursPerWeek, *role.HoursPerWeek))
	}
	if role.Timezone == nil && role.MinOverlapHours != nil {
		// The API and the roles table refuse such a role; if one exists
		// anyway, its requirement cannot be checked and so is not met.
		v.Reasons = append(v.Reasons, fmt.Sprintf("This role needs %d hours of overlap but has no time zone to work it out in", *role.MinOverlapHours))
	}
	if role.Timezone != nil {
		roleLoc, errRole := location(*role.Timezone)
		candLoc, errCand := location(c.Timezone)
		switch {
		case errCand != nil:
			// The API validates zones on the way in; a row that slipped past
			// must not pass by being unreadable.
			v.Reasons = append(v.Reasons, fmt.Sprintf("Time zone %q is not one the overlap can be worked out for", c.Timezone))
		case errRole != nil:
			v.Reasons = append(v.Reasons, fmt.Sprintf("The role's time zone %q is not one the overlap can be worked out for", *role.Timezone))
		default:
			minutes := OverlapMinutes(roleLoc, candLoc, c.WorkStart, c.WorkEnd, on)
			v.OverlapMinutes = &minutes
			if role.MinOverlapHours != nil && minutes < *role.MinOverlapHours*60 {
				v.Reasons = append(v.Reasons, fmt.Sprintf(
					"Working hours overlap the role's day (09:00 to 17:00 %s) by %s; this role needs %d",
					*role.Timezone, hours(minutes), *role.MinOverlapHours))
			}
		}
	}
	v.Passed = len(v.Reasons) == 0
	return v
}

// OverlapMinutes is how many minutes of the role's working day on the given
// day (09:00 to 17:00 in roleLoc) fall inside the candidate's working hours
// (start to end, minutes after midnight in candLoc; an end at or before the
// start runs past midnight). The candidate's hours repeat daily, so a night
// shift in Manila covers a morning in New York.
//
// It asks, for each minute of the role's day, what the candidate's clock
// reads. That is slower than intersecting intervals and right on the days a
// zone changes its clocks, when a local time can happen twice or not at all.
func OverlapMinutes(roleLoc, candLoc *time.Location, start, end int, on time.Time) int {
	y, m, d := on.Date()
	dayStart := time.Date(y, m, d, 0, RoleDayStart, 0, 0, roleLoc)
	dayEnd := time.Date(y, m, d, 0, RoleDayEnd, 0, 0, roleLoc)

	total := 0
	for t := dayStart; t.Before(dayEnd); t = t.Add(time.Minute) {
		local := t.In(candLoc)
		clock := local.Hour()*60 + local.Minute()
		working := clock >= start && clock < end
		if end <= start {
			working = clock >= start || clock < end
		}
		if working {
			total++
		}
	}
	return total
}

// zones caches loaded locations by name: a filter run checks hundreds of
// candidates against the same few zones, and LoadLocation parses the zone's
// rules on every call.
var zones sync.Map

func location(name string) (*time.Location, error) {
	if loc, ok := zones.Load(name); ok {
		return loc.(*time.Location), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	zones.Store(name, loc)
	return loc, nil
}

// day drops the time and zone, so two calendar days compare as days.
func day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// hours writes minutes as "3 hours", "1 hour" or "2.5 hours".
func hours(minutes int) string {
	if minutes == 60 {
		return "1 hour"
	}
	if minutes%60 == 0 {
		return fmt.Sprintf("%d hours", minutes/60)
	}
	return strconv.FormatFloat(float64(minutes)/60, 'f', -1, 64) + " hours"
}
