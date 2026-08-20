// Package testsupport provides shared fixtures for Ratiba's tests.
//
// It exists so that every test describes the same clinic. A failure that says
// "expected 14 slots" is only meaningful if the reader knows which doctor and
// which working hours produced it, and repeating that setup in each test file
// guarantees the copies drift apart.
package testsupport

import (
	"time"

	"github.com/JerryLegend254/ratiba/internal/doctor"
)

// Weekdays is the Monday-to-Friday set.
var Weekdays = []time.Weekday{
	time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday,
}

// Hours builds working-hours entries for the given weekdays.
func Hours(startHour, startMinute, endHour, endMinute int, days ...time.Weekday) []doctor.WorkingHours {
	hours := make([]doctor.WorkingHours, 0, len(days))
	for _, day := range days {
		hours = append(hours, doctor.WorkingHours{
			Weekday: day,
			Start:   doctor.LocalTime{Hour: startHour, Minute: startMinute},
			End:     doctor.LocalTime{Hour: endHour, Minute: endMinute},
		})
	}
	return hours
}

// MustLocation loads an IANA zone or panics. Only for test setup, where a
// missing zone database is a broken environment rather than a runtime error.
func MustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic("testsupport: cannot load location " + name + ": " + err.Error())
	}
	return loc
}
