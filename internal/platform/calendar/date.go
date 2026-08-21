// Package calendar provides a timezone-free calendar date.
//
// Availability is requested for a *local calendar day* ("2026-09-01 in the
// doctor's timezone"), which is not an instant and not a time.Time. Modelling
// it as its own type stops the two concepts being mixed up, and stops a bare
// time.Time carrying a meaningless location around the codebase.
package calendar

import (
	"fmt"
	"time"
)

// Layout is the ISO-8601 calendar date layout accepted on the wire.
const Layout = "2006-01-02"

// Date is a year-month-day with no timezone and no time-of-day.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// ParseDate parses a strict YYYY-MM-DD date. Go's time.Parse normalises
// out-of-range components (2026-02-30 becomes 2026-03-02), so the parsed value
// is re-formatted and compared to reject inputs that only look like dates.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(Layout, s)
	if err != nil {
		return Date{}, fmt.Errorf("parse date %q: %w", s, err)
	}
	if t.Format(Layout) != s {
		return Date{}, fmt.Errorf("parse date %q: not a real calendar date", s)
	}
	return Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
}

// DateOf returns the calendar date on which t falls, as observed in t's own
// location. Callers are responsible for putting t in the right location first.
func DateOf(t time.Time) Date {
	y, m, d := t.Date()
	return Date{Year: y, Month: m, Day: d}
}

// String renders the date as YYYY-MM-DD.
func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day)
}

// AtTime returns the instant at which the given wall-clock hour and minute
// occur on this date in loc.
//
// Around a DST transition this is not a total function: a wall clock time that
// is skipped by a spring-forward does not exist, and one that is repeated by a
// fall-back happens twice. time.Date resolves both cases deterministically
// (normalising forward, and choosing the first of two candidate offsets), which
// is the behaviour Ratiba documents and tests.
func (d Date) AtTime(hour, minute int, loc *time.Location) time.Time {
	return time.Date(d.Year, d.Month, d.Day, hour, minute, 0, 0, loc)
}

// Weekday reports the day of the week this date falls on. It is independent of
// timezone: a calendar date has the same weekday everywhere.
func (d Date) Weekday() time.Weekday {
	return d.AtTime(12, 0, time.UTC).Weekday()
}

// Equal reports whether two dates are the same day.
func (d Date) Equal(other Date) bool { return d == other }
