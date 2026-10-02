package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/colehanke/mavi-demo/api/internal/availability"
	"github.com/colehanke/mavi-demo/api/internal/contract"
	"github.com/colehanke/mavi-demo/api/internal/store"
)

// The ranges of the role's and the candidate's hours; the same bounds as the
// CHECK constraints in the migration and the minimum / maximum in the spec.
const (
	maxHoursPerWeek = 80
	maxOverlapHours = (availability.RoleDayEnd - availability.RoleDayStart) / 60
)

// availabilityInput validates a WorkAvailabilityInput. All four answers are
// required: a half-filled row would be a candidate who looks as if they had
// answered, and the filter passes only what was actually said.
func availabilityInput(b contract.WorkAvailabilityInput) (store.AvailabilityInput, error) {
	v := &validationError{}
	var in store.AvailabilityInput

	if tz := strPtr(b.Timezone); tz == nil {
		v.add("timezone", "required")
	} else {
		in.Timezone = *tz
		validTimezone(v, "timezone", tz)
	}

	clock := func(field string, p *contract.ClockTime) (string, int) {
		if p == nil {
			v.add(field, "required")
			return "", -1
		}
		minutes, ok := availability.ParseClock(string(*p))
		if !ok {
			v.add(field, "must be a time of day as HH:MM, e.g. 09:00")
			return "", -1
		}
		return string(*p), minutes
	}
	var start, end int
	in.WorkStart, start = clock("work_start", b.WorkStart)
	in.WorkEnd, end = clock("work_end", b.WorkEnd)
	if start >= 0 && start == end {
		v.add("work_end", "must differ from work_start")
	}

	switch {
	case b.HoursPerWeek == nil:
		v.add("hours_per_week", "required")
	case *b.HoursPerWeek < 1 || *b.HoursPerWeek > maxHoursPerWeek:
		v.add("hours_per_week", fmt.Sprintf("must be between 1 and %d", maxHoursPerWeek))
	default:
		in.HoursPerWeek = *b.HoursPerWeek
	}

	if b.AvailableFrom == nil {
		v.add("available_from", "required")
	} else {
		in.AvailableFrom = *b.AvailableFrom
	}
	return in, v.err()
}

func (s *Server) getAvailability(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ownsCandidate(w, r, id) {
		return
	}
	a, err := s.store.GetAvailability(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) putAvailability(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ownsCandidate(w, r, id) {
		return
	}
	var in contract.WorkAvailabilityInput
	if _, ok := decodeBody(w, r, &in, false); !ok {
		return
	}
	input, err := availabilityInput(in)
	if err != nil {
		fail(w, err)
		return
	}
	a, inserted, err := s.store.UpsertAvailability(r.Context(), id, input)
	if errors.Is(err, store.ErrBadRef) {
		// The candidate is the path resource, so a dangling reference is a 404.
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	code := http.StatusOK
	if inserted {
		code = http.StatusCreated
	}
	writeJSON(w, code, a)
}

// listRoleAvailability is the availability hard filter for one role, with
// the reason each excluded candidate is excluded.
func (s *Server) listRoleAvailability(w http.ResponseWriter, r *http.Request) {
	role, err := s.store.GetRole(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	// The overlap that matters is the one on the job: worked out for the
	// role's start date when it has one, so the answer does not change with
	// the day it is asked on.
	on := time.Now()
	if role.StartsOn != nil {
		on = time.Time(*role.StartsOn)
	}
	out, err := s.store.AvailabilityFilter(r.Context(), role, on, pageFrom(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
