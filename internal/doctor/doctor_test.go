package doctor_test

import (
	"testing"
	"time"

	"github.com/JerryLegend254/ratiba/internal/doctor"
	"github.com/JerryLegend254/ratiba/internal/platform/apperror"
	"github.com/JerryLegend254/ratiba/internal/platform/calendar"
)

func TestDoctorLocation(t *testing.T) {
	t.Parallel()

	t.Run("resolves a valid IANA zone", func(t *testing.T) {
		t.Parallel()

		loc, err := (doctor.Doctor{Timezone: "Africa/Nairobi"}).Location()
		if err != nil {
			t.Fatalf("expected the zone to resolve, got %v", err)
		}
		if loc.String() != "Africa/Nairobi" {
			t.Errorf("expected Africa/Nairobi, got %s", loc)
		}
	})

	t.Run("an unresolvable zone is a typed internal error", func(t *testing.T) {
		t.Parallel()

		_, err := (doctor.Doctor{Timezone: "Mars/Olympus_Mons"}).Location()
		if err == nil {
			t.Fatal("expected an error")
		}
		appErr, ok := apperror.From(err)
		if !ok {
			t.Fatalf("expected an *apperror.Error, got %T", err)
		}
		if appErr.Code != apperror.CodeUnsupportedTimezone {
			t.Errorf("expected %s, got %s", apperror.CodeUnsupportedTimezone, appErr.Code)
		}
		// It is a data problem, not something the caller did wrong.
		if appErr.Kind != apperror.KindInternal {
			t.Errorf("expected an internal kind, got %s", appErr.Kind)
		}
	})

	t.Run("the zone database is embedded in the binary", func(t *testing.T) {
		t.Parallel()

		// The production image is built FROM scratch with no system tzdata, so
		// this failing would mean every booking breaks in the container while
		// passing on a developer's machine.
		for _, name := range []string{"Africa/Nairobi", "Europe/London", "America/New_York", "Asia/Kathmandu"} {
			if _, err := (doctor.Doctor{Timezone: name}).Location(); err != nil {
				t.Errorf("zone %s is not available: %v", name, err)
			}
		}
	})
}

func TestParseLocalTime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input      string
		wantErr    bool
		wantMinute int
		// wantString is the canonical rendering; it defaults to input.
		wantString string
	}{
		{input: "09:00", wantMinute: 540},
		{input: "00:00", wantMinute: 0},
		{input: "23:59", wantMinute: 1439},
		{input: "08:30", wantMinute: 510},
		// Go's "15" layout accepts an unpadded hour. That is harmless — the
		// value means the same thing — but the canonical form is always padded,
		// which is what the database stores and the API renders.
		{input: "9:00", wantMinute: 540, wantString: "09:00"},

		{input: "24:00", wantErr: true},
		{input: "09:60", wantErr: true},
		{input: "09:00:00", wantErr: true},
		{input: "", wantErr: true},
		{input: "noon", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()

			parsed, err := doctor.ParseLocalTime(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected %q to be rejected", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected %q to parse, got %v", tc.input, err)
			}
			if parsed.MinuteOfDay() != tc.wantMinute {
				t.Errorf("expected %d minutes past midnight, got %d", tc.wantMinute, parsed.MinuteOfDay())
			}
			wantString := tc.wantString
			if wantString == "" {
				wantString = tc.input
			}
			if parsed.String() != wantString {
				t.Errorf("expected %q to render as %q, got %q", tc.input, wantString, parsed.String())
			}
		})
	}
}

func TestScheduleWindowsOn(t *testing.T) {
	t.Parallel()

	nairobi, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		t.Fatalf("load timezone: %v", err)
	}

	schedule := doctor.Schedule{
		Doctor: doctor.Doctor{Timezone: "Africa/Nairobi"},
		WorkingHours: []doctor.WorkingHours{
			// Deliberately out of order, to prove the result gets sorted.
			{Weekday: time.Monday, Start: doctor.LocalTime{Hour: 14}, End: doctor.LocalTime{Hour: 17}},
			{Weekday: time.Monday, Start: doctor.LocalTime{Hour: 9}, End: doctor.LocalTime{Hour: 13}},
			{Weekday: time.Wednesday, Start: doctor.LocalTime{Hour: 10}, End: doctor.LocalTime{Hour: 12}},
		},
	}

	t.Run("returns only the requested weekday, in order", func(t *testing.T) {
		t.Parallel()

		windows := schedule.WindowsOn(calendar.Date{Year: 2026, Month: time.September, Day: 7}, nairobi)
		if len(windows) != 2 {
			t.Fatalf("expected 2 windows on Monday, got %d", len(windows))
		}
		if got := windows[0].Start.In(nairobi).Format("15:04"); got != "09:00" {
			t.Errorf("expected the first window to start at 09:00, got %s", got)
		}
		if got := windows[1].Start.In(nairobi).Format("15:04"); got != "14:00" {
			t.Errorf("expected the second window to start at 14:00, got %s", got)
		}
	})

	t.Run("a day with no working hours yields nothing", func(t *testing.T) {
		t.Parallel()

		windows := schedule.WindowsOn(calendar.Date{Year: 2026, Month: time.September, Day: 8}, nairobi)
		if len(windows) != 0 {
			t.Fatalf("expected no windows on Tuesday, got %d", len(windows))
		}
	})

	t.Run("windows carry absolute instants derived from the doctor's zone", func(t *testing.T) {
		t.Parallel()

		windows := schedule.WindowsOn(calendar.Date{Year: 2026, Month: time.September, Day: 7}, nairobi)
		if got := windows[0].Start.UTC().Format(time.RFC3339); got != "2026-09-07T06:00:00Z" {
			t.Errorf("expected 09:00 Nairobi to be 06:00Z, got %s", got)
		}
		if got := windows[0].End.UTC().Format(time.RFC3339); got != "2026-09-07T10:00:00Z" {
			t.Errorf("expected 13:00 Nairobi to be 10:00Z, got %s", got)
		}
	})

	t.Run("a DST transition changes the window's real duration", func(t *testing.T) {
		t.Parallel()

		london, err := time.LoadLocation("Europe/London")
		if err != nil {
			t.Fatalf("load timezone: %v", err)
		}

		// Europe/London springs forward at 01:00 UTC on 2026-03-29, inside this
		// deliberately unusual overnight-adjacent window. Eight wall-clock
		// hours are only seven real ones, and the slot generator must see that.
		overnight := doctor.Schedule{
			Doctor: doctor.Doctor{Timezone: "Europe/London"},
			WorkingHours: []doctor.WorkingHours{
				{Weekday: time.Sunday, Start: doctor.LocalTime{Hour: 0}, End: doctor.LocalTime{Hour: 8}},
			},
		}

		windows := overnight.WindowsOn(calendar.Date{Year: 2026, Month: time.March, Day: 29}, london)
		if len(windows) != 1 {
			t.Fatalf("expected 1 window, got %d", len(windows))
		}
		if got := windows[0].End.Sub(windows[0].Start); got != 7*time.Hour {
			t.Errorf("expected 7 real hours across the spring-forward, got %s", got)
		}
	})
}

func TestDoctorErrorsAreStable(t *testing.T) {
	t.Parallel()

	if code := doctor.ErrNotFound().Code; code != apperror.CodeDoctorNotFound {
		t.Errorf("expected %s, got %s", apperror.CodeDoctorNotFound, code)
	}
	if kind := doctor.ErrNotFound().Kind; kind != apperror.KindNotFound {
		t.Errorf("expected a not-found kind, got %s", kind)
	}
	if code := doctor.ErrInactive().Code; code != apperror.CodeDoctorInactive {
		t.Errorf("expected %s, got %s", apperror.CodeDoctorInactive, code)
	}
	// Inactive is a business rule violation, not a missing resource.
	if kind := doctor.ErrInactive().Kind; kind != apperror.KindUnprocessable {
		t.Errorf("expected an unprocessable kind, got %s", kind)
	}
}
