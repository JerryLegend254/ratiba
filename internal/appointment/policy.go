package appointment

import (
	"fmt"
	"time"

	"github.com/JerryLegend254/ratiba/internal/doctor"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/calendar"
)

// DefaultSlotDuration is the fixed consultation length.
const DefaultSlotDuration = 30 * time.Minute

// DefaultMinLeadTime is how far in the future a booking must be.
const DefaultMinLeadTime = time.Hour

// Policy is the complete, self-contained set of rules that decide whether a
// given instant is a bookable slot start for a given doctor.
//
// Booking, rescheduling and availability all run through this one type. That is
// the point: a slot offered by GET /doctors/{id}/availability is bookable by
// construction, and a reschedule destination is validated by exactly the same
// code path as a fresh booking, so the three endpoints cannot drift apart.
type Policy struct {
	// SlotDuration is the length of every appointment.
	SlotDuration time.Duration
	// MinLeadTime is the minimum gap between "now" and a slot's start. A slot
	// is allowed when start >= now + MinLeadTime.
	MinLeadTime time.Duration
}

// NewPolicy validates and returns a Policy.
//
// SlotDuration must divide an hour evenly. If it did not, slot starts generated
// from a :00 or :30 working-hours boundary would wander off the half-hour grid
// partway through the day and the ":00 or :30 only" guarantee would be a lie.
func NewPolicy(slotDuration, minLeadTime time.Duration) (Policy, error) {
	if slotDuration <= 0 {
		return Policy{}, fmt.Errorf("slot duration must be positive, got %s", slotDuration)
	}
	if time.Hour%slotDuration != 0 {
		return Policy{}, fmt.Errorf("slot duration %s must divide one hour evenly", slotDuration)
	}
	if minLeadTime < 0 {
		return Policy{}, fmt.Errorf("minimum lead time must not be negative, got %s", minLeadTime)
	}
	return Policy{SlotDuration: slotDuration, MinLeadTime: minLeadTime}, nil
}

// DefaultPolicy is the policy described in the README: 30-minute slots booked at
// least one hour ahead.
func DefaultPolicy() Policy {
	return Policy{SlotDuration: DefaultSlotDuration, MinLeadTime: DefaultMinLeadTime}
}

// SlotsIn returns every whole slot that fits inside the window, in order.
//
// Stepping is done in absolute time, so each slot is exactly SlotDuration of
// real elapsed time. A slot is only emitted if it fits entirely within
// [w.Start, w.End); this is what stops a consultation from running past the end
// of a doctor's working hours, and it is also what keeps a DST-shortened window
// from over-producing slots.
func (p Policy) SlotsIn(w doctor.Window) []Slot {
	slots := make([]Slot, 0, 16)
	for start := w.Start; ; start = start.Add(p.SlotDuration) {
		end := start.Add(p.SlotDuration)
		if end.After(w.End) {
			break
		}
		slots = append(slots, Slot{Start: start, End: end})
	}
	return slots
}

// SlotAt returns the slot that begins at start.
//
// Every write path builds its interval through here rather than adding the
// duration inline, so the half-open [start, start+duration) convention is
// applied in exactly one place.
func (p Policy) SlotAt(start time.Time) Slot {
	return Slot{Start: start, End: start.Add(p.SlotDuration)}
}

// SlotsOn returns every slot the doctor's schedule defines on the given local
// calendar date, ignoring existing bookings and lead time.
//
// This is the definition of "aligned": a start is aligned precisely when it
// appears in this list. Deriving alignment from generation rather than from a
// separate modulo check means the two can never disagree, including across DST
// transitions.
func (p Policy) SlotsOn(schedule doctor.Schedule, date calendar.Date, loc *time.Location) []Slot {
	var slots []Slot
	for _, window := range schedule.WindowsOn(date, loc) {
		slots = append(slots, p.SlotsIn(window)...)
	}
	return slots
}

// BookableAt reports whether a slot starting at start is far enough in the
// future, given the current instant.
func (p Policy) BookableAt(now, start time.Time) bool {
	return !start.Before(now.Add(p.MinLeadTime))
}

// FreeSlotsOn returns the slots a patient may actually book on the given local
// date: every scheduled slot, minus those already taken, minus those that fall
// inside the lead-time window.
//
// booked holds the start instants of active appointments; it is compared by
// instant, so the caller's timezone is irrelevant.
func (p Policy) FreeSlotsOn(
	schedule doctor.Schedule,
	date calendar.Date,
	loc *time.Location,
	now time.Time,
	booked []time.Time,
) []Slot {
	taken := make(map[int64]struct{}, len(booked))
	for _, b := range booked {
		taken[b.UTC().UnixNano()] = struct{}{}
	}

	all := p.SlotsOn(schedule, date, loc)
	free := make([]Slot, 0, len(all))
	for _, slot := range all {
		if _, ok := taken[slot.Start.UTC().UnixNano()]; ok {
			continue
		}
		if !p.BookableAt(now, slot.Start) {
			continue
		}
		free = append(free, slot)
	}
	return free
}

// ValidateStart decides whether start is a legal slot start for this schedule
// at this moment, and returns a precise, stable error when it is not.
//
// It does NOT consider whether the slot is already taken. That question is
// answered only by the database's partial unique index, because any answer
// derived from a prior read is stale the instant it is produced.
//
// Checks run in this order, and the first failure wins:
//
//  1. temporal — is the start in the past, or inside the lead-time window?
//  2. structural — does the doctor work that day, is the start on the slot
//     grid, and does the whole appointment fit inside working hours?
//
// Temporal comes first deliberately: "you cannot book last Tuesday" is a more
// useful answer than "the doctor does not work on Tuesdays", especially since a
// schedule may have changed since the date in question.
func (p Policy) ValidateStart(
	schedule doctor.Schedule,
	loc *time.Location,
	now time.Time,
	start time.Time,
) error {
	if start.Before(now) {
		return apperror.New(apperror.KindUnprocessable, apperror.CodeSlotInPast,
			"The requested slot is in the past.")
	}
	if !p.BookableAt(now, start) {
		return apperror.Newf(apperror.KindUnprocessable, apperror.CodeSlotTooSoon,
			"Appointments must start at least %s from now.", humanDuration(p.MinLeadTime))
	}

	date := calendar.DateOf(start.In(loc))
	windows := schedule.WindowsOn(date, loc)
	if len(windows) == 0 {
		return apperror.Newf(apperror.KindUnprocessable, apperror.CodeDoctorNotWorking,
			"The doctor does not work on %s.", date)
	}

	for _, slot := range p.SlotsOn(schedule, date, loc) {
		if slot.Start.Equal(start) {
			return nil
		}
	}

	// Not a valid start. Distinguish "off the grid" from "outside the hours" so
	// the caller learns something actionable.
	for _, window := range windows {
		if start.Before(window.Start) || !start.Before(window.End) {
			continue
		}
		// Inside a working window. If the start sits on the grid, the only
		// remaining reason it was rejected is that the appointment would run
		// past the end of the window.
		if offset := start.Sub(window.Start); offset%p.SlotDuration == 0 {
			return apperror.Newf(apperror.KindUnprocessable, apperror.CodeSlotOutsideHours,
				"A %s appointment starting then would end after the doctor's working hours.",
				humanDuration(p.SlotDuration))
		}
		return apperror.Newf(apperror.KindUnprocessable, apperror.CodeSlotNotAligned,
			"Appointments must start on a %s boundary within the doctor's working hours.",
			humanDuration(p.SlotDuration))
	}

	return apperror.New(apperror.KindUnprocessable, apperror.CodeSlotOutsideHours,
		"The requested slot is outside the doctor's working hours.")
}

// SearchRange returns the half-open instant range that encloses every slot the
// schedule defines on date, or ok=false when the doctor does not work that day.
//
// Availability uses this to fetch existing bookings with a bounded query
// instead of guessing at local midnight, which is not a safe anchor in every
// timezone (some zones have historically shifted at midnight, so that wall
// clock time does not always exist).
func (p Policy) SearchRange(
	schedule doctor.Schedule,
	date calendar.Date,
	loc *time.Location,
) (from, to time.Time, ok bool) {
	slots := p.SlotsOn(schedule, date, loc)
	if len(slots) == 0 {
		return time.Time{}, time.Time{}, false
	}
	from = slots[0].Start
	to = slots[len(slots)-1].End
	return from, to, true
}

// humanDuration renders a duration the way an API message should read
// ("1 hour", "30 minutes") rather than the way Go prints it ("1h0m0s").
func humanDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0 && d >= time.Hour:
		hours := int(d / time.Hour)
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	case d%time.Minute == 0:
		minutes := int(d / time.Minute)
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	default:
		return d.String()
	}
}
