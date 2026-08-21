package appointment_test

import (
	"errors"
	"testing"
	"time"

	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/doctor"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/calendar"
	"github.com/JerryLegend254/ratiba/internal/testsupport"
)

// nairobi is UTC+3 year round, so a local hour maps to a fixed UTC hour and the
// expectations below stay readable.
func nairobiSchedule() doctor.Schedule {
	return doctor.Schedule{
		Doctor: doctor.Doctor{Timezone: "Africa/Nairobi", IsActive: true},
		WorkingHours: append(
			testsupport.Hours(9, 0, 13, 0, testsupport.Weekdays...),
			testsupport.Hours(14, 0, 17, 0, testsupport.Weekdays...)...,
		),
	}
}

// nairobiTZ is resolved once. MustLocation panics if the zone database is
// missing, which is a broken environment rather than a test failure.
var nairobiTZ = testsupport.MustLocation("Africa/Nairobi")

// at builds an instant from a Nairobi wall-clock time in September 2026.
func at(day, hour, minute int) time.Time {
	return time.Date(2026, 9, day, hour, minute, 0, 0, nairobiTZ)
}

// errorCode extracts the stable code from a domain error.
func errorCode(t *testing.T, err error) string {
	t.Helper()
	var appErr *apperror.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected an *apperror.Error, got %T: %v", err, err)
	}
	return appErr.Code
}

func TestPolicyValidateStart(t *testing.T) {
	t.Parallel()

	schedule := nairobiSchedule()
	nairobi := nairobiTZ
	policy := appointment.DefaultPolicy()

	// Monday 2026-09-07, 06:00 local — three hours before the clinic opens, so
	// every slot that day clears the one-hour lead time.
	now := at(7, 6, 0)

	tests := []struct {
		name  string
		start time.Time
		// wantCode is "" when the slot must be accepted.
		wantCode string
	}{
		{
			name:  "first slot of the morning session",
			start: at(7, 9, 0),
		},
		{
			name:  "last slot that still fits before the morning break",
			start: at(7, 12, 30),
		},
		{
			name:  "first slot of the afternoon session",
			start: at(7, 14, 0),
		},
		{
			name:  "last slot of the day",
			start: at(7, 16, 30),
		},
		{
			name:     "slot starting exactly at closing time does not fit",
			start:    at(7, 17, 0),
			wantCode: apperror.CodeSlotOutsideHours,
		},
		{
			name:     "slot starting at the end of the morning session does not fit",
			start:    at(7, 13, 0),
			wantCode: apperror.CodeSlotOutsideHours,
		},
		{
			name:     "before opening",
			start:    at(7, 8, 30),
			wantCode: apperror.CodeSlotOutsideHours,
		},
		{
			name:     "inside the lunch gap",
			start:    at(7, 13, 30),
			wantCode: apperror.CodeSlotOutsideHours,
		},
		{
			name:     "after closing",
			start:    at(7, 18, 0),
			wantCode: apperror.CodeSlotOutsideHours,
		},
		{
			name:     "misaligned by fifteen minutes",
			start:    at(7, 9, 15),
			wantCode: apperror.CodeSlotNotAligned,
		},
		{
			name:     "misaligned by one minute",
			start:    at(7, 10, 1),
			wantCode: apperror.CodeSlotNotAligned,
		},
		{
			name:     "weekend, doctor does not work",
			start:    at(12, 9, 0), // Saturday
			wantCode: apperror.CodeDoctorNotWorking,
		},
		{
			name:     "in the past",
			start:    at(4, 9, 0), // the previous Friday
			wantCode: apperror.CodeSlotInPast,
		},
		{
			name:     "inside the one-hour lead time",
			start:    at(7, 9, 0).Add(-3*time.Hour + 30*time.Minute), // 06:30, 30 min away
			wantCode: apperror.CodeSlotTooSoon,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := policy.ValidateStart(schedule, nairobi, now, tc.start)

			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected %s to be bookable, got %v", tc.start.Format(time.RFC3339), err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %s to be rejected with %s, got no error",
					tc.start.Format(time.RFC3339), tc.wantCode)
			}
			if got := errorCode(t, err); got != tc.wantCode {
				t.Fatalf("expected code %s, got %s (%v)", tc.wantCode, got, err)
			}
		})
	}
}

// TestPolicyLeadTimeBoundary pins the exact boundary of the one-hour rule.
// "At least one hour" must mean start >= now+1h, so a slot exactly one hour
// away is allowed and one a second closer is not.
func TestPolicyLeadTimeBoundary(t *testing.T) {
	t.Parallel()

	schedule := nairobiSchedule()
	nairobi := nairobiTZ
	policy := appointment.DefaultPolicy()
	start := at(7, 10, 0)

	tests := []struct {
		name    string
		now     time.Time
		allowed bool
	}{
		{name: "exactly one hour before", now: start.Add(-time.Hour), allowed: true},
		{name: "one second more than an hour before", now: start.Add(-time.Hour - time.Second), allowed: true},
		{name: "one second less than an hour before", now: start.Add(-time.Hour + time.Second), allowed: false},
		{name: "thirty minutes before", now: start.Add(-30 * time.Minute), allowed: false},
		{name: "at the start instant", now: start, allowed: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := policy.ValidateStart(schedule, nairobi, tc.now, start)
			if tc.allowed {
				if err != nil {
					t.Fatalf("expected the slot to be allowed, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected the slot to be rejected")
			}
			if code := errorCode(t, err); code != apperror.CodeSlotTooSoon {
				t.Fatalf("expected %s, got %s", apperror.CodeSlotTooSoon, code)
			}
		})
	}
}

func TestPolicySlotsOn(t *testing.T) {
	t.Parallel()

	schedule := nairobiSchedule()
	nairobi := nairobiTZ
	policy := appointment.DefaultPolicy()

	t.Run("a full working day yields every whole slot in both sessions", func(t *testing.T) {
		t.Parallel()

		slots := policy.SlotsOn(schedule, calendar.Date{Year: 2026, Month: time.September, Day: 7}, nairobi)

		// 09:00-13:00 is eight slots, 14:00-17:00 is six.
		if len(slots) != 14 {
			t.Fatalf("expected 14 slots, got %d", len(slots))
		}
		if got := slots[0].Start.In(nairobi).Format("15:04"); got != "09:00" {
			t.Errorf("expected the first slot at 09:00, got %s", got)
		}
		if got := slots[7].Start.In(nairobi).Format("15:04"); got != "12:30" {
			t.Errorf("expected the last morning slot at 12:30, got %s", got)
		}
		if got := slots[8].Start.In(nairobi).Format("15:04"); got != "14:00" {
			t.Errorf("expected the first afternoon slot at 14:00, got %s", got)
		}
		if got := slots[13].Start.In(nairobi).Format("15:04"); got != "16:30" {
			t.Errorf("expected the last slot at 16:30, got %s", got)
		}
	})

	t.Run("slots are contiguous, ordered and exactly thirty minutes", func(t *testing.T) {
		t.Parallel()

		slots := policy.SlotsOn(schedule, calendar.Date{Year: 2026, Month: time.September, Day: 7}, nairobi)
		for i, slot := range slots {
			if got := slot.End.Sub(slot.Start); got != 30*time.Minute {
				t.Fatalf("slot %d lasts %s, expected 30m", i, got)
			}
			if i > 0 && !slots[i-1].Start.Before(slot.Start) {
				t.Fatalf("slot %d is not after slot %d", i, i-1)
			}
		}
	})

	t.Run("a non-working day yields nothing", func(t *testing.T) {
		t.Parallel()

		slots := policy.SlotsOn(schedule, calendar.Date{Year: 2026, Month: time.September, Day: 12}, nairobi)
		if len(slots) != 0 {
			t.Fatalf("expected no slots on Saturday, got %d", len(slots))
		}
	})
}

// TestPolicyFreeSlotsOn checks the three exclusion rules availability applies.
func TestPolicyFreeSlotsOn(t *testing.T) {
	t.Parallel()

	schedule := nairobiSchedule()
	nairobi := nairobiTZ
	policy := appointment.DefaultPolicy()
	date := calendar.Date{Year: 2026, Month: time.September, Day: 7}

	t.Run("booked slots are excluded", func(t *testing.T) {
		t.Parallel()

		booked := []time.Time{at(7, 9, 0), at(7, 10, 30)}
		free := policy.FreeSlotsOn(schedule, date, nairobi, at(7, 6, 0), booked)

		if len(free) != 12 {
			t.Fatalf("expected 12 free slots after two bookings, got %d", len(free))
		}
		for _, slot := range free {
			for _, taken := range booked {
				if slot.Start.Equal(taken) {
					t.Fatalf("booked slot %s was offered as free", taken.Format(time.RFC3339))
				}
			}
		}
	})

	t.Run("slots inside the lead-time window are excluded", func(t *testing.T) {
		t.Parallel()

		// 11:15 local: 11:30 and 12:00 are within the hour, 12:30 is not.
		free := policy.FreeSlotsOn(schedule, date, nairobi, at(7, 11, 15), nil)

		if len(free) == 0 {
			t.Fatal("expected some slots to remain")
		}
		if got := free[0].Start.In(nairobi).Format("15:04"); got != "12:30" {
			t.Fatalf("expected the first bookable slot at 12:30, got %s", got)
		}
	})

	t.Run("every offered slot passes validation", func(t *testing.T) {
		t.Parallel()

		// The guarantee that makes availability trustworthy: anything the
		// endpoint offers must be accepted by the booking validator.
		now := at(7, 6, 0)
		for _, slot := range policy.FreeSlotsOn(schedule, date, nairobi, now, nil) {
			if err := policy.ValidateStart(schedule, nairobi, now, slot.Start); err != nil {
				t.Fatalf("availability offered %s but validation rejected it: %v",
					slot.Start.Format(time.RFC3339), err)
			}
		}
	})
}

// TestPolicyAcrossDSTTransition covers a doctor in a zone that changes offset.
//
// Europe/London moves from GMT to BST at 01:00 UTC on the last Sunday in March.
// The transition happens outside working hours, so the wall clock is stable and
// the UTC instants shift by an hour — which is exactly the behaviour a patient
// expects: "my 09:00 appointment" stays at 09:00 local.
func TestPolicyAcrossDSTTransition(t *testing.T) {
	t.Parallel()

	london := testsupport.MustLocation("Europe/London")
	schedule := doctor.Schedule{
		Doctor:       doctor.Doctor{Timezone: "Europe/London", IsActive: true},
		WorkingHours: testsupport.Hours(9, 0, 17, 0, testsupport.Weekdays...),
	}
	policy := appointment.DefaultPolicy()

	// 2026-03-29 is the last Sunday in March; Friday the 27th is GMT and
	// Monday the 30th is BST.
	beforeDST := policy.SlotsOn(schedule, calendar.Date{Year: 2026, Month: time.March, Day: 27}, london)
	afterDST := policy.SlotsOn(schedule, calendar.Date{Year: 2026, Month: time.March, Day: 30}, london)

	if len(beforeDST) != len(afterDST) {
		t.Fatalf("expected the same slot count either side of the DST change, got %d and %d",
			len(beforeDST), len(afterDST))
	}

	t.Run("local wall clock is unchanged", func(t *testing.T) {
		t.Parallel()
		if got := beforeDST[0].Start.In(london).Format("15:04"); got != "09:00" {
			t.Errorf("before DST: expected 09:00 local, got %s", got)
		}
		if got := afterDST[0].Start.In(london).Format("15:04"); got != "09:00" {
			t.Errorf("after DST: expected 09:00 local, got %s", got)
		}
	})

	t.Run("the UTC instant shifts by the offset change", func(t *testing.T) {
		t.Parallel()
		// 09:00 GMT is 09:00Z; 09:00 BST is 08:00Z.
		if got := beforeDST[0].Start.UTC().Format("15:04"); got != "09:00" {
			t.Errorf("before DST: expected 09:00Z, got %sZ", got)
		}
		if got := afterDST[0].Start.UTC().Format("15:04"); got != "08:00" {
			t.Errorf("after DST: expected 08:00Z, got %sZ", got)
		}
	})

	t.Run("a slot booked in local terms validates in either period", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC)
		start := time.Date(2026, 3, 30, 9, 0, 0, 0, london)
		if err := policy.ValidateStart(schedule, london, now, start); err != nil {
			t.Fatalf("expected 09:00 BST to be bookable, got %v", err)
		}
	})
}

func TestNewPolicyRejectsUnusableDurations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		slotDuration time.Duration
		leadTime     time.Duration
		wantErr      bool
	}{
		{name: "thirty minutes", slotDuration: 30 * time.Minute, leadTime: time.Hour},
		{name: "fifteen minutes", slotDuration: 15 * time.Minute, leadTime: time.Hour},
		{name: "zero lead time is allowed", slotDuration: 30 * time.Minute, leadTime: 0},
		{name: "zero duration", slotDuration: 0, wantErr: true},
		{name: "negative duration", slotDuration: -time.Minute, wantErr: true},
		// 45 minutes would slide slots off the half-hour grid partway through
		// the day, breaking the ":00 or :30 only" guarantee.
		{name: "does not divide an hour", slotDuration: 45 * time.Minute, wantErr: true},
		{name: "negative lead time", slotDuration: 30 * time.Minute, leadTime: -time.Hour, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := appointment.NewPolicy(tc.slotDuration, tc.leadTime)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestPolicySearchRange(t *testing.T) {
	t.Parallel()

	schedule := nairobiSchedule()
	nairobi := nairobiTZ
	policy := appointment.DefaultPolicy()

	t.Run("spans the first slot start to the last slot end", func(t *testing.T) {
		t.Parallel()

		from, to, ok := policy.SearchRange(schedule, calendar.Date{Year: 2026, Month: time.September, Day: 7}, nairobi)
		if !ok {
			t.Fatal("expected a range on a working day")
		}
		if got := from.In(nairobi).Format("15:04"); got != "09:00" {
			t.Errorf("expected the range to start at 09:00, got %s", got)
		}
		if got := to.In(nairobi).Format("15:04"); got != "17:00" {
			t.Errorf("expected the range to end at 17:00, got %s", got)
		}
	})

	t.Run("reports no range on a non-working day", func(t *testing.T) {
		t.Parallel()

		if _, _, ok := policy.SearchRange(schedule, calendar.Date{Year: 2026, Month: time.September, Day: 12}, nairobi); ok {
			t.Fatal("expected no range on Saturday")
		}
	})
}
