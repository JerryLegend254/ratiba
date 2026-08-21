package calendar_test

import (
	"testing"
	"time"

	"github.com/JerryLegend254/ratiba/internal/platform/calendar"
)

func TestParseDate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "an ordinary date", input: "2026-09-07"},
		{name: "a leap day in a leap year", input: "2024-02-29"},
		{name: "the first day of a year", input: "2026-01-01"},
		{name: "the last day of a year", input: "2026-12-31"},

		{name: "empty", input: "", wantErr: true},
		{name: "day-first format", input: "07-09-2026", wantErr: true},
		{name: "slashes", input: "2026/09/07", wantErr: true},
		{name: "unpadded month", input: "2026-9-7", wantErr: true},
		{name: "with a time", input: "2026-09-07T00:00:00Z", wantErr: true},
		{name: "not a date at all", input: "tomorrow", wantErr: true},
		// Go's time.Parse silently normalises these into the following month,
		// which would quietly return availability for the wrong day.
		{name: "31st of a 30-day month", input: "2026-09-31", wantErr: true},
		{name: "30th of February", input: "2026-02-30", wantErr: true},
		{name: "29th of February in a common year", input: "2026-02-29", wantErr: true},
		{name: "month 13", input: "2026-13-01", wantErr: true},
		{name: "day zero", input: "2026-09-00", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			date, err := calendar.ParseDate(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected %q to be rejected, got %s", tc.input, date)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected %q to parse, got %v", tc.input, err)
			}
			if date.String() != tc.input {
				t.Errorf("round trip changed %q into %q", tc.input, date.String())
			}
		})
	}
}

func TestDateWeekdayIsTimezoneIndependent(t *testing.T) {
	t.Parallel()

	// A calendar date falls on the same weekday everywhere; only instants have
	// a timezone. This is what lets working hours be looked up by weekday
	// without knowing where the caller is.
	date := calendar.Date{Year: 2026, Month: time.September, Day: 7}
	if got := date.Weekday(); got != time.Monday {
		t.Errorf("expected 2026-09-07 to be a Monday, got %s", got)
	}

	for _, name := range []string{"UTC", "Africa/Nairobi", "Pacific/Kiritimati", "Pacific/Niue"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if got := calendar.DateOf(date.AtTime(12, 0, loc)).Weekday(); got != time.Monday {
			t.Errorf("in %s the weekday came out as %s", name, got)
		}
	}
}

func TestDateAtTime(t *testing.T) {
	t.Parallel()

	nairobi, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		t.Fatalf("load timezone: %v", err)
	}

	t.Run("applies the location's offset", func(t *testing.T) {
		t.Parallel()

		date := calendar.Date{Year: 2026, Month: time.September, Day: 7}
		instant := date.AtTime(9, 0, nairobi)

		if got := instant.UTC().Format(time.RFC3339); got != "2026-09-07T06:00:00Z" {
			t.Errorf("expected 09:00 in Nairobi to be 06:00Z, got %s", got)
		}
	})

	t.Run("the same wall clock is a different instant in a different zone", func(t *testing.T) {
		t.Parallel()

		date := calendar.Date{Year: 2026, Month: time.September, Day: 7}
		if date.AtTime(9, 0, nairobi).Equal(date.AtTime(9, 0, time.UTC)) {
			t.Error("09:00 Nairobi and 09:00 UTC must not be the same instant")
		}
	})
}

func TestDateOf(t *testing.T) {
	t.Parallel()

	// An instant near midnight belongs to different calendar days depending on
	// the zone it is observed in. Availability depends on getting this right:
	// the doctor's zone decides which day a slot belongs to.
	instant := time.Date(2026, 9, 7, 22, 30, 0, 0, time.UTC)

	nairobi, err := time.LoadLocation("Africa/Nairobi") // UTC+3
	if err != nil {
		t.Fatalf("load timezone: %v", err)
	}

	if got := calendar.DateOf(instant.In(time.UTC)).String(); got != "2026-09-07" {
		t.Errorf("in UTC the date should be 2026-09-07, got %s", got)
	}
	if got := calendar.DateOf(instant.In(nairobi)).String(); got != "2026-09-08" {
		t.Errorf("in Nairobi that instant is already the next day, got %s", got)
	}
}

func TestDateString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		date calendar.Date
		want string
	}{
		{calendar.Date{Year: 2026, Month: time.September, Day: 7}, "2026-09-07"},
		{calendar.Date{Year: 2026, Month: time.December, Day: 31}, "2026-12-31"},
		{calendar.Date{Year: 999, Month: time.January, Day: 1}, "0999-01-01"},
	}

	for _, tc := range tests {
		if got := tc.date.String(); got != tc.want {
			t.Errorf("expected %s, got %s", tc.want, got)
		}
	}
}
