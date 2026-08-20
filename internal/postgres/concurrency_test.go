//go:build integration

package postgres_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/JerryLegend254/ratiba/internal/appointment"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
)

// These are the tests the whole design exists to satisfy. The claim being
// checked is narrow and absolute: no matter how many requests race for one
// doctor's slot, the database ends up with exactly one active appointment for
// it, and every loser is told so with a 409-mapped conflict.

// TestExactlyTwoConcurrentBookingsForTheSameSlot is the scenario from the
// brief: two patients hit "book" on the same slot at the same moment.
func TestExactlyTwoConcurrentBookingsForTheSameSlot(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()
	start := slotAt(t, 10, 0)

	type outcome struct {
		result appointment.BookResult
		err    error
	}
	results := make([]outcome, 2)

	// A wait group used as a starting gate: both goroutines block until the
	// main goroutine releases them, so the two INSERTs genuinely overlap
	// instead of running one after the other.
	var gate sync.WaitGroup
	gate.Add(1)

	var finished sync.WaitGroup
	for i := range 2 {
		finished.Add(1)
		go func() {
			defer finished.Done()
			gate.Wait()

			result, err := f.service.Book(ctx, appointment.BookCommand{
				DoctorID:  f.doctorID,
				PatientID: f.patients[i],
				StartsAt:  start,
			})
			results[i] = outcome{result: result, err: err}
		}()
	}

	gate.Done()
	finished.Wait()

	var succeeded, conflicted int
	for i, out := range results {
		switch {
		case out.err == nil:
			succeeded++
		case isCode(out.err, apperror.CodeSlotUnavailable):
			conflicted++
		default:
			t.Fatalf("attempt %d failed with an unexpected error: %v", i, out.err)
		}
	}

	if succeeded != 1 {
		t.Errorf("expected exactly 1 successful booking, got %d", succeeded)
	}
	if conflicted != 1 {
		t.Errorf("expected exactly 1 conflict, got %d", conflicted)
	}

	// The assertion that actually matters: the database holds one row, not two.
	if count := countActive(t, f.doctorID, start); count != 1 {
		t.Fatalf("expected exactly 1 active appointment in the database, found %d", count)
	}
}

// TestManyConcurrentBookingsForTheSameSlot raises the contention well beyond
// anything the brief requires. The invariant must not depend on how many
// requests arrive.
func TestManyConcurrentBookingsForTheSameSlot(t *testing.T) {
	t.Parallel()

	const attempts = 24

	f := newFixture(t)
	ctx := t.Context()
	start := slotAt(t, 11, 0)

	errs := make([]error, attempts)

	var gate sync.WaitGroup
	gate.Add(1)
	var finished sync.WaitGroup

	for i := range attempts {
		finished.Add(1)
		go func() {
			defer finished.Done()
			gate.Wait()

			// Alternating patients means the winner is not predetermined by
			// which patient row the transaction happens to touch.
			_, err := f.service.Book(ctx, appointment.BookCommand{
				DoctorID:  f.doctorID,
				PatientID: f.patients[i%len(f.patients)],
				StartsAt:  start,
			})
			errs[i] = err
		}()
	}

	gate.Done()
	finished.Wait()

	var succeeded, conflicted int
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case isCode(err, apperror.CodeSlotUnavailable):
			conflicted++
		default:
			t.Fatalf("attempt %d failed with an unexpected error: %v", i, err)
		}
	}

	if succeeded != 1 {
		t.Errorf("expected exactly 1 winner out of %d, got %d", attempts, succeeded)
	}
	if conflicted != attempts-1 {
		t.Errorf("expected %d conflicts, got %d", attempts-1, conflicted)
	}
	if count := countActive(t, f.doctorID, start); count != 1 {
		t.Fatalf("expected exactly 1 active appointment, found %d", count)
	}
}

// TestConcurrentBookingsForDifferentSlotsAllSucceed guards the opposite
// failure: an over-broad lock that serialises unrelated bookings would make
// this fail, and would be invisible in a single-threaded test.
func TestConcurrentBookingsForDifferentSlotsAllSucceed(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	starts := []struct{ hour, minute int }{
		{9, 0}, {9, 30}, {10, 0}, {10, 30}, {11, 0}, {11, 30},
	}
	errs := make([]error, len(starts))

	var gate sync.WaitGroup
	gate.Add(1)
	var finished sync.WaitGroup

	for i, s := range starts {
		finished.Add(1)
		go func() {
			defer finished.Done()
			gate.Wait()

			_, err := f.service.Book(ctx, appointment.BookCommand{
				DoctorID:  f.doctorID,
				PatientID: f.patients[i%len(f.patients)],
				StartsAt:  slotAt(t, s.hour, s.minute),
			})
			errs[i] = err
		}()
	}

	gate.Done()
	finished.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("booking %d:%02d failed but should have succeeded: %v",
				starts[i].hour, starts[i].minute, err)
		}
	}
	for _, s := range starts {
		if count := countActive(t, f.doctorID, slotAt(t, s.hour, s.minute)); count != 1 {
			t.Errorf("expected 1 appointment at %d:%02d, found %d", s.hour, s.minute, count)
		}
	}
}

// TestConcurrentCancellations proves the row lock serialises cancellations, so
// only one caller can be the one who cancelled it.
func TestConcurrentCancellations(t *testing.T) {
	t.Parallel()

	const attempts = 6

	f := newFixture(t)
	ctx := t.Context()

	booked, err := f.service.Book(ctx, appointment.BookCommand{
		DoctorID: f.doctorID, PatientID: f.patients[0], StartsAt: slotAt(t, 13, 0),
	})
	if err != nil {
		t.Fatalf("setup booking failed: %v", err)
	}

	errs := make([]error, attempts)

	var gate sync.WaitGroup
	gate.Add(1)
	var finished sync.WaitGroup

	for i := range attempts {
		finished.Add(1)
		go func() {
			defer finished.Done()
			gate.Wait()
			_, errs[i] = f.service.Cancel(ctx, appointment.CancelCommand{
				ID: booked.Appointment.ID, Reason: "concurrent cancellation test",
			})
		}()
	}

	gate.Done()
	finished.Wait()

	var succeeded, conflicted int
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case isCode(err, apperror.CodeAlreadyCancelled):
			conflicted++
		default:
			t.Fatalf("cancellation %d failed unexpectedly: %v", i, err)
		}
	}

	if succeeded != 1 {
		t.Errorf("expected exactly 1 cancellation to succeed, got %d", succeeded)
	}
	if conflicted != attempts-1 {
		t.Errorf("expected %d conflicts, got %d", attempts-1, conflicted)
	}
	// Exactly one 'booked' event and one 'cancelled' event.
	if events := countEvents(t, booked.Appointment.ID); events != 2 {
		t.Fatalf("expected 2 audit events, found %d", events)
	}
}

// TestConcurrentReschedulesOntoOneSlot has several appointments race for a
// single free destination. One must win; the losers must stay exactly where
// they were, not end up slotless.
func TestConcurrentReschedulesOntoOneSlot(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()
	destination := slotAt(t, 15, 0)

	sources := []int{9, 10, 11}
	appointments := make([]appointment.Appointment, 0, len(sources))
	for _, hour := range sources {
		booked, err := f.service.Book(ctx, appointment.BookCommand{
			DoctorID: f.doctorID, PatientID: f.patients[0], StartsAt: slotAt(t, hour, 0),
		})
		if err != nil {
			t.Fatalf("setup booking at %d:00 failed: %v", hour, err)
		}
		appointments = append(appointments, booked.Appointment)
	}

	errs := make([]error, len(appointments))

	var gate sync.WaitGroup
	gate.Add(1)
	var finished sync.WaitGroup

	for i, appt := range appointments {
		finished.Add(1)
		go func() {
			defer finished.Done()
			gate.Wait()
			_, errs[i] = f.service.Reschedule(ctx, appointment.RescheduleCommand{
				ID: appt.ID, StartsAt: destination,
			})
		}()
	}

	gate.Done()
	finished.Wait()

	var succeeded int
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case isCode(err, apperror.CodeSlotUnavailable):
			// Loser: must still hold its original slot.
			current, getErr := f.service.Get(ctx, appointments[i].ID)
			if getErr != nil {
				t.Fatalf("could not re-read appointment %d: %v", i, getErr)
			}
			if !current.StartsAt.Equal(appointments[i].StartsAt) {
				t.Errorf("a failed reschedule moved appointment %d from %s to %s",
					i, appointments[i].StartsAt, current.StartsAt)
			}
			if current.Status != appointment.StatusBooked {
				t.Errorf("a failed reschedule left appointment %d in status %s", i, current.Status)
			}
		default:
			t.Fatalf("reschedule %d failed unexpectedly: %v", i, err)
		}
	}

	if succeeded != 1 {
		t.Errorf("expected exactly 1 reschedule to win the destination, got %d", succeeded)
	}
	if count := countActive(t, f.doctorID, destination); count != 1 {
		t.Fatalf("expected exactly 1 appointment at the destination, found %d", count)
	}
}

// isCode reports whether err carries the given stable API code.
func isCode(err error, code string) bool {
	var appErr *apperror.Error
	return errors.As(err, &appErr) && appErr.Code == code
}
