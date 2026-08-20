package appointment_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/calendar"
	"github.com/JerryLegend254/ratiba/internal/platform/clock"
	"github.com/JerryLegend254/ratiba/internal/testsupport"
)

// newFixture builds a service over the standard in-memory clinic with the clock
// pinned to Monday 2026-09-07 05:00 UTC (08:00 in Nairobi).
func newFixture(t *testing.T) (*appointment.Service, *testsupport.MemoryStore, *clock.Fixed) {
	t.Helper()

	store := testsupport.NewClinic()
	clk := testsupport.NewFixedClock()
	service, err := testsupport.NewService(store, clk)
	if err != nil {
		t.Fatalf("build service: %v", err)
	}
	return service, store, clk
}

// nairobiAt builds an instant from a Nairobi wall-clock time on Monday
// 2026-09-07, the day the fixed test clock sits on.
func nairobiAt(hour, minute int) time.Time {
	return time.Date(2026, 9, 7, hour, minute, 0, 0, nairobiTZ)
}

func TestServiceBook(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("books a valid slot and records an audit event", func(t *testing.T) {
		t.Parallel()
		service, store, _ := newFixture(t)

		result, err := service.Book(ctx, appointment.BookCommand{
			DoctorID:  testsupport.NairobiDoctorID,
			PatientID: testsupport.ActivePatientID,
			StartsAt:  nairobiAt(9, 0),
		})
		if err != nil {
			t.Fatalf("expected the booking to succeed, got %v", err)
		}
		if result.Appointment.Status != appointment.StatusBooked {
			t.Errorf("expected status booked, got %s", result.Appointment.Status)
		}
		if got := result.Appointment.EndsAt.Sub(result.Appointment.StartsAt); got != 30*time.Minute {
			t.Errorf("expected a 30 minute appointment, got %s", got)
		}

		events := store.Events()
		if len(events) != 1 || events[0].Type != appointment.EventBooked {
			t.Fatalf("expected one 'booked' event, got %+v", events)
		}
		if events[0].AppointmentID != result.Appointment.ID {
			t.Error("the audit event does not reference the created appointment")
		}
	})

	t.Run("rejects a slot another patient already holds", func(t *testing.T) {
		t.Parallel()
		service, store, _ := newFixture(t)
		start := nairobiAt(9, 0)

		if _, err := service.Book(ctx, appointment.BookCommand{
			DoctorID: testsupport.NairobiDoctorID, PatientID: testsupport.ActivePatientID, StartsAt: start,
		}); err != nil {
			t.Fatalf("first booking failed: %v", err)
		}

		_, err := service.Book(ctx, appointment.BookCommand{
			DoctorID: testsupport.NairobiDoctorID, PatientID: testsupport.OtherPatientID, StartsAt: start,
		})
		if code := errorCode(t, err); code != apperror.CodeSlotUnavailable {
			t.Fatalf("expected %s, got %s", apperror.CodeSlotUnavailable, code)
		}
		if count := store.ActiveCount(testsupport.NairobiDoctorID, start); count != 1 {
			t.Fatalf("expected exactly one active appointment for the slot, found %d", count)
		}
	})

	t.Run("the same slot with a different doctor is fine", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)
		start := nairobiAt(12, 0)

		if _, err := service.Book(ctx, appointment.BookCommand{
			DoctorID: testsupport.NairobiDoctorID, PatientID: testsupport.ActivePatientID, StartsAt: start,
		}); err != nil {
			t.Fatalf("first booking failed: %v", err)
		}
		// The London doctor works 09:00-17:00 local, which covers 12:00 Nairobi
		// (10:00 London in September).
		if _, err := service.Book(ctx, appointment.BookCommand{
			DoctorID: testsupport.LondonDoctorID, PatientID: testsupport.ActivePatientID, StartsAt: start,
		}); err != nil {
			t.Fatalf("expected a different doctor to be bookable at the same instant, got %v", err)
		}
	})

	t.Run("rejects unknown and inactive participants", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name      string
			doctorID  uuid.UUID
			patientID uuid.UUID
			wantCode  string
		}{
			{
				name:      "unknown doctor",
				doctorID:  uuid.MustParse("00000000-0000-4000-8000-000000000099"),
				patientID: testsupport.ActivePatientID,
				wantCode:  apperror.CodeDoctorNotFound,
			},
			{
				name:      "unknown patient",
				doctorID:  testsupport.NairobiDoctorID,
				patientID: uuid.MustParse("00000000-0000-4000-8000-000000000098"),
				wantCode:  apperror.CodePatientNotFound,
			},
			{
				name:      "inactive doctor",
				doctorID:  testsupport.InactiveDoctorID,
				patientID: testsupport.ActivePatientID,
				wantCode:  apperror.CodeDoctorInactive,
			},
			{
				name:      "inactive patient",
				doctorID:  testsupport.NairobiDoctorID,
				patientID: testsupport.InactivePatientID,
				wantCode:  apperror.CodePatientInactive,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				service, _, _ := newFixture(t)

				_, err := service.Book(ctx, appointment.BookCommand{
					DoctorID: tc.doctorID, PatientID: tc.patientID, StartsAt: nairobiAt(9, 0),
				})
				if code := errorCode(t, err); code != tc.wantCode {
					t.Fatalf("expected %s, got %s", tc.wantCode, code)
				}
			})
		}
	})

	t.Run("a failed transaction leaves no partial state", func(t *testing.T) {
		t.Parallel()
		service, store, _ := newFixture(t)
		start := nairobiAt(9, 0)

		store.FailWith = errors.New("simulated commit failure")

		if _, err := service.Book(ctx, appointment.BookCommand{
			DoctorID: testsupport.NairobiDoctorID, PatientID: testsupport.ActivePatientID, StartsAt: start,
		}); err == nil {
			t.Fatal("expected the booking to fail")
		}

		if count := store.ActiveCount(testsupport.NairobiDoctorID, start); count != 0 {
			t.Fatalf("expected no appointment after a failed transaction, found %d", count)
		}
		if events := store.Events(); len(events) != 0 {
			t.Fatalf("expected no audit events after a failed transaction, found %d", len(events))
		}
	})
}

func TestServiceCancel(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	book := func(t *testing.T, service *appointment.Service, hour int) appointment.Appointment {
		t.Helper()
		result, err := service.Book(ctx, appointment.BookCommand{
			DoctorID: testsupport.NairobiDoctorID, PatientID: testsupport.ActivePatientID,
			StartsAt: nairobiAt(hour, 0),
		})
		if err != nil {
			t.Fatalf("setup booking failed: %v", err)
		}
		return result.Appointment
	}

	t.Run("cancelling releases the slot for rebooking", func(t *testing.T) {
		t.Parallel()
		service, store, _ := newFixture(t)
		appt := book(t, service, 9)

		cancelled, err := service.Cancel(ctx, appointment.CancelCommand{
			ID: appt.ID, Reason: "Patient is travelling that week",
		})
		if err != nil {
			t.Fatalf("cancel failed: %v", err)
		}
		if cancelled.Status != appointment.StatusCancelled {
			t.Errorf("expected status cancelled, got %s", cancelled.Status)
		}
		if cancelled.CancelledAt == nil || cancelled.CancellationReason == nil {
			t.Fatal("a cancelled appointment must carry both a timestamp and a reason")
		}

		// The record is retained rather than deleted.
		if _, ok := store.Appointment(appt.ID); !ok {
			t.Error("the cancelled appointment must be retained for audit")
		}
		// And the slot is immediately free again.
		if _, err := service.Book(ctx, appointment.BookCommand{
			DoctorID: testsupport.NairobiDoctorID, PatientID: testsupport.OtherPatientID,
			StartsAt: appt.StartsAt,
		}); err != nil {
			t.Fatalf("expected the released slot to be bookable, got %v", err)
		}
	})

	t.Run("cancelling twice is a conflict", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)
		appt := book(t, service, 9)

		if _, err := service.Cancel(ctx, appointment.CancelCommand{ID: appt.ID, Reason: "first"}); err != nil {
			t.Fatalf("first cancel failed: %v", err)
		}
		_, err := service.Cancel(ctx, appointment.CancelCommand{ID: appt.ID, Reason: "second"})
		if code := errorCode(t, err); code != apperror.CodeAlreadyCancelled {
			t.Fatalf("expected %s, got %s", apperror.CodeAlreadyCancelled, code)
		}
	})

	t.Run("the reason is mandatory and bounded", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name   string
			reason string
		}{
			{name: "empty", reason: ""},
			{name: "whitespace only", reason: "   \t\n  "},
			{name: "too long", reason: strings.Repeat("a", appointment.MaxCancellationReasonLength+1)},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				service, _, _ := newFixture(t)
				appt := book(t, service, 9)

				_, err := service.Cancel(ctx, appointment.CancelCommand{ID: appt.ID, Reason: tc.reason})
				if code := errorCode(t, err); code != apperror.CodeValidationFailed {
					t.Fatalf("expected %s, got %s", apperror.CodeValidationFailed, code)
				}
			})
		}
	})

	t.Run("a reason at the exact limit is accepted and trimmed", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)
		appt := book(t, service, 9)

		reason := strings.Repeat("b", appointment.MaxCancellationReasonLength)
		cancelled, err := service.Cancel(ctx, appointment.CancelCommand{ID: appt.ID, Reason: "  " + reason + "  "})
		if err != nil {
			t.Fatalf("expected the maximum-length reason to be accepted, got %v", err)
		}
		if *cancelled.CancellationReason != reason {
			t.Error("expected surrounding whitespace to be trimmed from the reason")
		}
	})

	t.Run("an unknown appointment is not found", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)

		_, err := service.Cancel(ctx, appointment.CancelCommand{
			ID: uuid.MustParse("00000000-0000-4000-8000-000000000097"), Reason: "whatever",
		})
		if code := errorCode(t, err); code != apperror.CodeAppointmentNotFound {
			t.Fatalf("expected %s, got %s", apperror.CodeAppointmentNotFound, code)
		}
	})
}

func TestServiceReschedule(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	book := func(t *testing.T, service *appointment.Service, patientID uuid.UUID, hour int) appointment.Appointment {
		t.Helper()
		result, err := service.Book(ctx, appointment.BookCommand{
			DoctorID: testsupport.NairobiDoctorID, PatientID: patientID, StartsAt: nairobiAt(hour, 0),
		})
		if err != nil {
			t.Fatalf("setup booking failed: %v", err)
		}
		return result.Appointment
	}

	t.Run("moving frees the old slot and claims the new one", func(t *testing.T) {
		t.Parallel()
		service, store, _ := newFixture(t)
		appt := book(t, service, testsupport.ActivePatientID, 9)
		destination := nairobiAt(11, 0)

		moved, err := service.Reschedule(ctx, appointment.RescheduleCommand{
			ID: appt.ID, StartsAt: destination,
		})
		if err != nil {
			t.Fatalf("reschedule failed: %v", err)
		}
		if !moved.StartsAt.Equal(destination) {
			t.Errorf("expected the appointment at %s, got %s", destination, moved.StartsAt)
		}
		if moved.ID != appt.ID {
			t.Error("rescheduling must move the appointment, not replace it")
		}

		if count := store.ActiveCount(testsupport.NairobiDoctorID, appt.StartsAt); count != 0 {
			t.Error("the original slot should have been released")
		}
		if count := store.ActiveCount(testsupport.NairobiDoctorID, destination); count != 1 {
			t.Error("the destination slot should be held")
		}

		events := store.Events()
		last := events[len(events)-1]
		if last.Type != appointment.EventRescheduled {
			t.Fatalf("expected a 'rescheduled' event, got %s", last.Type)
		}
		if last.FromStartsAt == nil || !last.FromStartsAt.Equal(appt.StartsAt) {
			t.Error("the audit event must record where the appointment moved from")
		}
		if last.ToStartsAt == nil || !last.ToStartsAt.Equal(destination) {
			t.Error("the audit event must record where the appointment moved to")
		}
	})

	t.Run("a conflicting destination rolls back and leaves the original intact", func(t *testing.T) {
		t.Parallel()
		service, store, _ := newFixture(t)

		mine := book(t, service, testsupport.ActivePatientID, 9)
		theirs := book(t, service, testsupport.OtherPatientID, 11)

		_, err := service.Reschedule(ctx, appointment.RescheduleCommand{
			ID: mine.ID, StartsAt: theirs.StartsAt,
		})
		if code := errorCode(t, err); code != apperror.CodeSlotUnavailable {
			t.Fatalf("expected %s, got %s", apperror.CodeSlotUnavailable, code)
		}

		// The critical assertion: a failed move must not leave the appointment
		// slotless or cancelled.
		current, ok := store.Appointment(mine.ID)
		if !ok {
			t.Fatal("the appointment disappeared after a failed reschedule")
		}
		if !current.StartsAt.Equal(mine.StartsAt) {
			t.Errorf("expected the appointment to stay at %s, found it at %s",
				mine.StartsAt, current.StartsAt)
		}
		if current.Status != appointment.StatusBooked {
			t.Errorf("expected the appointment to remain booked, got %s", current.Status)
		}
		if count := store.ActiveCount(testsupport.NairobiDoctorID, theirs.StartsAt); count != 1 {
			t.Error("the other patient's appointment must be untouched")
		}
	})

	t.Run("moving to the current slot is a conflict", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)
		appt := book(t, service, testsupport.ActivePatientID, 9)

		_, err := service.Reschedule(ctx, appointment.RescheduleCommand{
			ID: appt.ID, StartsAt: appt.StartsAt,
		})
		if code := errorCode(t, err); code != apperror.CodeRescheduleSameSlot {
			t.Fatalf("expected %s, got %s", apperror.CodeRescheduleSameSlot, code)
		}
	})

	t.Run("a cancelled appointment cannot be moved", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)
		appt := book(t, service, testsupport.ActivePatientID, 9)

		if _, err := service.Cancel(ctx, appointment.CancelCommand{ID: appt.ID, Reason: "no longer needed"}); err != nil {
			t.Fatalf("cancel failed: %v", err)
		}

		_, err := service.Reschedule(ctx, appointment.RescheduleCommand{
			ID: appt.ID, StartsAt: nairobiAt(11, 0),
		})
		if code := errorCode(t, err); code != apperror.CodeAlreadyCancelled {
			t.Fatalf("expected %s, got %s", apperror.CodeAlreadyCancelled, code)
		}
	})

	t.Run("the destination is validated exactly like a new booking", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			start    time.Time
			wantCode string
		}{
			{
				name:     "misaligned",
				start:    nairobiAt(11, 15),
				wantCode: apperror.CodeSlotNotAligned,
			},
			{
				name:     "outside working hours",
				start:    nairobiAt(20, 0),
				wantCode: apperror.CodeSlotOutsideHours,
			},
			{
				name:     "in the past",
				start:    time.Date(2026, 9, 4, 9, 0, 0, 0, nairobiTZ),
				wantCode: apperror.CodeSlotInPast,
			},
			{
				name:     "inside the lead-time window",
				start:    nairobiAt(8, 30),
				wantCode: apperror.CodeSlotTooSoon,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				service, store, _ := newFixture(t)
				appt := book(t, service, testsupport.ActivePatientID, 9)

				_, err := service.Reschedule(ctx, appointment.RescheduleCommand{
					ID: appt.ID, StartsAt: tc.start,
				})
				if code := errorCode(t, err); code != tc.wantCode {
					t.Fatalf("expected %s, got %s", tc.wantCode, code)
				}
				current, _ := store.Appointment(appt.ID)
				if !current.StartsAt.Equal(appt.StartsAt) {
					t.Error("a rejected reschedule must not move the appointment")
				}
			})
		}
	})
}

func TestServiceAvailability(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("excludes booked slots and keeps a stable order", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)

		before, err := service.Availability(ctx, appointment.AvailabilityQuery{
			DoctorID: testsupport.NairobiDoctorID,
			Date:     calendar.Date{Year: 2026, Month: time.September, Day: 7},
		})
		if err != nil {
			t.Fatalf("availability failed: %v", err)
		}
		if len(before.Slots) != 14 {
			t.Fatalf("expected 14 slots on an empty day, got %d", len(before.Slots))
		}

		if _, err := service.Book(ctx, appointment.BookCommand{
			DoctorID: testsupport.NairobiDoctorID, PatientID: testsupport.ActivePatientID,
			StartsAt: before.Slots[3].Start,
		}); err != nil {
			t.Fatalf("booking failed: %v", err)
		}

		after, err := service.Availability(ctx, appointment.AvailabilityQuery{
			DoctorID: testsupport.NairobiDoctorID,
			Date:     calendar.Date{Year: 2026, Month: time.September, Day: 7},
		})
		if err != nil {
			t.Fatalf("availability failed: %v", err)
		}
		if len(after.Slots) != 13 {
			t.Fatalf("expected 13 slots after one booking, got %d", len(after.Slots))
		}
		for _, slot := range after.Slots {
			if slot.Start.Equal(before.Slots[3].Start) {
				t.Fatal("the booked slot is still being offered")
			}
		}
		for i := 1; i < len(after.Slots); i++ {
			if !after.Slots[i-1].Start.Before(after.Slots[i].Start) {
				t.Fatal("slots must be returned in ascending order")
			}
		}
	})

	t.Run("a cancelled appointment returns its slot to availability", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)
		date := calendar.Date{Year: 2026, Month: time.September, Day: 7}

		booked, err := service.Book(ctx, appointment.BookCommand{
			DoctorID: testsupport.NairobiDoctorID, PatientID: testsupport.ActivePatientID,
			StartsAt: nairobiAt(10, 0),
		})
		if err != nil {
			t.Fatalf("booking failed: %v", err)
		}
		if _, err := service.Cancel(ctx, appointment.CancelCommand{
			ID: booked.Appointment.ID, Reason: "double booked at work",
		}); err != nil {
			t.Fatalf("cancel failed: %v", err)
		}

		result, err := service.Availability(ctx, appointment.AvailabilityQuery{
			DoctorID: testsupport.NairobiDoctorID, Date: date,
		})
		if err != nil {
			t.Fatalf("availability failed: %v", err)
		}
		if len(result.Slots) != 14 {
			t.Fatalf("expected all 14 slots after cancellation, got %d", len(result.Slots))
		}
	})

	t.Run("a non-working day is an empty list, not an error", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)

		result, err := service.Availability(ctx, appointment.AvailabilityQuery{
			DoctorID: testsupport.NairobiDoctorID,
			Date:     calendar.Date{Year: 2026, Month: time.September, Day: 12}, // Saturday
		})
		if err != nil {
			t.Fatalf("expected an empty result rather than an error, got %v", err)
		}
		if len(result.Slots) != 0 {
			t.Fatalf("expected no slots, got %d", len(result.Slots))
		}
	})

	t.Run("an inactive doctor is rejected consistently with booking", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)

		_, err := service.Availability(ctx, appointment.AvailabilityQuery{
			DoctorID: testsupport.InactiveDoctorID,
			Date:     calendar.Date{Year: 2026, Month: time.September, Day: 7},
		})
		if code := errorCode(t, err); code != apperror.CodeDoctorInactive {
			t.Fatalf("expected %s, got %s", apperror.CodeDoctorInactive, code)
		}
	})

	t.Run("an unknown doctor is not found", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)

		_, err := service.Availability(ctx, appointment.AvailabilityQuery{
			DoctorID: uuid.MustParse("00000000-0000-4000-8000-000000000096"),
			Date:     calendar.Date{Year: 2026, Month: time.September, Day: 7},
		})
		if code := errorCode(t, err); code != apperror.CodeDoctorNotFound {
			t.Fatalf("expected %s, got %s", apperror.CodeDoctorNotFound, code)
		}
	})

	t.Run("advancing the clock shrinks availability through the lead-time rule", func(t *testing.T) {
		t.Parallel()
		service, _, clk := newFixture(t)
		date := calendar.Date{Year: 2026, Month: time.September, Day: 7}

		first, err := service.Availability(ctx, appointment.AvailabilityQuery{
			DoctorID: testsupport.NairobiDoctorID, Date: date,
		})
		if err != nil {
			t.Fatalf("availability failed: %v", err)
		}

		// Move to 11:00 Nairobi. Slots before 12:00 are now inside the hour.
		clk.Set(nairobiAt(11, 0))

		second, err := service.Availability(ctx, appointment.AvailabilityQuery{
			DoctorID: testsupport.NairobiDoctorID, Date: date,
		})
		if err != nil {
			t.Fatalf("availability failed: %v", err)
		}
		if len(second.Slots) >= len(first.Slots) {
			t.Fatalf("expected fewer slots later in the day, got %d then %d",
				len(first.Slots), len(second.Slots))
		}
		if got := second.Slots[0].Start; got.Before(clk.Now().Add(time.Hour)) {
			t.Errorf("the first offered slot %s is inside the lead-time window", got)
		}
	})
}

func TestServiceListUpcomingForPatient(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("returns future active appointments in chronological order", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)

		// Book out of order to prove the sort is real.
		for _, hour := range []int{15, 9, 11} {
			if _, err := service.Book(ctx, appointment.BookCommand{
				DoctorID: testsupport.NairobiDoctorID, PatientID: testsupport.ActivePatientID,
				StartsAt: nairobiAt(hour, 0),
			}); err != nil {
				t.Fatalf("booking at %d:00 failed: %v", hour, err)
			}
		}

		result, err := service.ListUpcomingForPatient(ctx, appointment.PatientAppointmentsQuery{
			PatientID: testsupport.ActivePatientID,
		})
		if err != nil {
			t.Fatalf("listing failed: %v", err)
		}
		if result.Total != 3 {
			t.Fatalf("expected 3 appointments, got %d", result.Total)
		}
		for i := 1; i < len(result.Items); i++ {
			if result.Items[i-1].Appointment.StartsAt.After(result.Items[i].Appointment.StartsAt) {
				t.Fatal("appointments must be ordered by start time ascending")
			}
		}
		if result.Items[0].Doctor.FullName == "" {
			t.Error("each item should carry its doctor so a client need not fetch them separately")
		}
	})

	t.Run("excludes cancelled and past appointments", func(t *testing.T) {
		t.Parallel()
		service, store, clk := newFixture(t)

		keep, err := service.Book(ctx, appointment.BookCommand{
			DoctorID: testsupport.NairobiDoctorID, PatientID: testsupport.ActivePatientID,
			StartsAt: nairobiAt(16, 0),
		})
		if err != nil {
			t.Fatalf("booking failed: %v", err)
		}
		drop, err := service.Book(ctx, appointment.BookCommand{
			DoctorID: testsupport.NairobiDoctorID, PatientID: testsupport.ActivePatientID,
			StartsAt: nairobiAt(10, 0),
		})
		if err != nil {
			t.Fatalf("booking failed: %v", err)
		}
		if _, err := service.Cancel(ctx, appointment.CancelCommand{
			ID: drop.Appointment.ID, Reason: "no longer needed",
		}); err != nil {
			t.Fatalf("cancel failed: %v", err)
		}

		// A past appointment, inserted directly since booking one is impossible.
		store.AddAppointment(appointment.Appointment{
			ID:        uuid.MustParse("00000000-0000-4000-8000-0000000000aa"),
			DoctorID:  testsupport.NairobiDoctorID,
			PatientID: testsupport.ActivePatientID,
			StartsAt:  nairobiAt(9, 0).AddDate(0, 0, -7),
			EndsAt:    nairobiAt(9, 30).AddDate(0, 0, -7),
			Status:    appointment.StatusBooked,
		})

		clk.Set(nairobiAt(12, 0))

		result, err := service.ListUpcomingForPatient(ctx, appointment.PatientAppointmentsQuery{
			PatientID: testsupport.ActivePatientID,
		})
		if err != nil {
			t.Fatalf("listing failed: %v", err)
		}
		if result.Total != 1 {
			t.Fatalf("expected only the one upcoming active appointment, got %d", result.Total)
		}
		if result.Items[0].Appointment.ID != keep.Appointment.ID {
			t.Error("the wrong appointment was returned")
		}
	})

	t.Run("paging is bounded and reports the unpaged total", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)

		for _, hour := range []int{9, 10, 11, 12} {
			if _, err := service.Book(ctx, appointment.BookCommand{
				DoctorID: testsupport.NairobiDoctorID, PatientID: testsupport.ActivePatientID,
				StartsAt: nairobiAt(hour, 0),
			}); err != nil {
				t.Fatalf("booking failed: %v", err)
			}
		}

		page, err := service.ListUpcomingForPatient(ctx, appointment.PatientAppointmentsQuery{
			PatientID: testsupport.ActivePatientID,
			Page:      appointment.Page{Limit: 2, Offset: 0},
		})
		if err != nil {
			t.Fatalf("listing failed: %v", err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("expected 2 items on the page, got %d", len(page.Items))
		}
		if page.Total != 4 {
			t.Fatalf("expected a total of 4 regardless of paging, got %d", page.Total)
		}

		next, err := service.ListUpcomingForPatient(ctx, appointment.PatientAppointmentsQuery{
			PatientID: testsupport.ActivePatientID,
			Page:      appointment.Page{Limit: 2, Offset: 2},
		})
		if err != nil {
			t.Fatalf("listing failed: %v", err)
		}
		if len(next.Items) != 2 {
			t.Fatalf("expected 2 items on the second page, got %d", len(next.Items))
		}
		if next.Items[0].Appointment.ID == page.Items[0].Appointment.ID {
			t.Error("the second page repeated the first page's first item")
		}
	})

	t.Run("an over-large limit is clamped to the configured maximum", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)

		result, err := service.ListUpcomingForPatient(ctx, appointment.PatientAppointmentsQuery{
			PatientID: testsupport.ActivePatientID,
			Page:      appointment.Page{Limit: 10_000},
		})
		if err != nil {
			t.Fatalf("listing failed: %v", err)
		}
		if result.Limit != 100 {
			t.Fatalf("expected the limit to be clamped to 100, got %d", result.Limit)
		}
	})

	t.Run("an unknown patient is not found", func(t *testing.T) {
		t.Parallel()
		service, _, _ := newFixture(t)

		_, err := service.ListUpcomingForPatient(ctx, appointment.PatientAppointmentsQuery{
			PatientID: uuid.MustParse("00000000-0000-4000-8000-000000000095"),
		})
		if code := errorCode(t, err); code != apperror.CodePatientNotFound {
			t.Fatalf("expected %s, got %s", apperror.CodePatientNotFound, code)
		}
	})
}

func TestNewServiceRejectsBadWiring(t *testing.T) {
	t.Parallel()

	store := testsupport.NewClinic()
	valid := appointment.ServiceConfig{
		Policy:          appointment.DefaultPolicy(),
		DefaultPageSize: 20,
		MaxPageSize:     100,
	}

	tests := []struct {
		name string
		cfg  appointment.ServiceConfig
	}{
		{
			name: "default page size above the maximum",
			cfg: appointment.ServiceConfig{
				Policy:          appointment.DefaultPolicy(),
				DefaultPageSize: 200, MaxPageSize: 100,
			},
		},
		{
			name: "unusable slot duration",
			cfg: appointment.ServiceConfig{
				Policy:          appointment.Policy{SlotDuration: 45 * time.Minute, MinLeadTime: time.Hour},
				DefaultPageSize: 20, MaxPageSize: 100,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := appointment.NewService(
				store.Appointments(), store.Doctors(), store.Patients(),
				testsupport.NewFixedClock(), testsupport.Logger(), appointment.NopMetrics{}, tc.cfg,
			)
			if err == nil {
				t.Fatal("expected the service to refuse this configuration")
			}
		})
	}

	t.Run("a missing collaborator is refused", func(t *testing.T) {
		t.Parallel()
		_, err := appointment.NewService(
			nil, store.Doctors(), store.Patients(),
			testsupport.NewFixedClock(), testsupport.Logger(), appointment.NopMetrics{}, valid,
		)
		if err == nil {
			t.Fatal("expected a nil repository to be refused")
		}
	})
}
