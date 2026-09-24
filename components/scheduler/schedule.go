package scheduler

import (
	"fmt"
	"time"

	"github.com/vanclief/ez"
)

// Schedule is an immutable description of the wall-clock minutes at which a
// recurring job is due. Build it with Every, Hourly or Daily. The zero value is
// invalid. A value can be reused for several ids.
type Schedule struct {
	minutes [60]bool       // selected minutes of the hour
	hour    int            // Daily only
	loc     *time.Location // nil for Every/Hourly (server-local); non-nil for Daily
	err     error          // constructor validation error, reported by Add
}

// Every returns a Schedule that is due at minute 0 of every hour and every d
// after it, in server-local time. d must be a whole number of minutes that
// divides one hour evenly (1, 2, 3, 4, 5, 6, 10, 12, 15, 20, 30 or 60 minutes).
// Every(time.Hour) is equivalent to Hourly(0). An invalid d yields a Schedule
// that Add rejects with an ez.EINVALID error.
func Every(d time.Duration) Schedule {
	if d <= 0 || d%time.Minute != 0 || d > time.Hour || time.Hour%d != 0 {
		msg := fmt.Sprintf("scheduler: Every interval must be a whole number of minutes that divides one hour evenly, got %s", d)
		return Schedule{err: ez.New(ez.EINVALID, msg, nil)}
	}

	var s Schedule
	step := int(d / time.Minute)
	for m := 0; m < 60; m += step {
		s.minutes[m] = true
	}

	return s
}

// Hourly returns a Schedule that is due at each of the given minutes of every
// hour, in server-local time. At least one minute is required, each must be in
// 0..59 and none may repeat. Order does not matter. Invalid input yields a
// Schedule that Add rejects with an ez.EINVALID error.
func Hourly(minutes ...int) Schedule {
	if len(minutes) == 0 {
		return Schedule{err: ez.New(ez.EINVALID, "scheduler: Hourly requires at least one minute", nil)}
	}

	var s Schedule
	for _, m := range minutes {
		if m < 0 || m > 59 {
			msg := fmt.Sprintf("scheduler: Hourly minute must be in 0..59, got %d", m)
			return Schedule{err: ez.New(ez.EINVALID, msg, nil)}
		}
		if s.minutes[m] {
			msg := fmt.Sprintf("scheduler: Hourly minute %d given more than once", m)
			return Schedule{err: ez.New(ez.EINVALID, msg, nil)}
		}
		s.minutes[m] = true
	}

	return s
}

// Daily returns a Schedule that is due once a day at hour:minute wall-clock
// time in loc. hour must be in 0..23, minute in 0..59 and loc non-nil. On a day
// where that local time does not exist (DST spring-forward) the schedule is not
// due; on a day where it occurs twice (DST fall-back) it is due at both
// instants. Invalid input yields a Schedule that Add rejects with an ez.EINVALID
// error.
func Daily(hour, minute int, loc *time.Location) Schedule {
	if hour < 0 || hour > 23 {
		msg := fmt.Sprintf("scheduler: Daily hour must be in 0..23, got %d", hour)
		return Schedule{err: ez.New(ez.EINVALID, msg, nil)}
	}
	if minute < 0 || minute > 59 {
		msg := fmt.Sprintf("scheduler: Daily minute must be in 0..59, got %d", minute)
		return Schedule{err: ez.New(ez.EINVALID, msg, nil)}
	}
	if loc == nil {
		return Schedule{err: ez.New(ez.EINVALID, "scheduler: Daily location cannot be nil", nil)}
	}

	var s Schedule
	s.minutes[minute] = true
	s.hour = hour
	s.loc = loc

	return s
}

// validate returns nil when the schedule is usable. It returns the constructor
// error when one was recorded, and an ez.EINVALID error for the zero value
// (no minute selected and no error recorded).
func (s Schedule) validate() error {
	if s.err != nil {
		return s.err
	}

	for _, selected := range s.minutes {
		if selected {
			return nil
		}
	}

	return ez.New(ez.EINVALID, "scheduler: empty Schedule, build it with Every, Hourly or Daily", nil)
}

// matches reports whether the minute boundary t is selected. For a minute-set
// schedule (loc == nil) t is converted with t.Local() and only the minute is
// compared. For Daily (loc != nil) t is converted with t.In(s.loc) and the hour
// and the minute are compared. Seconds are ignored.
func (s Schedule) matches(t time.Time) bool {
	if s.loc == nil {
		return s.minutes[t.Local().Minute()]
	}

	local := t.In(s.loc)
	return local.Hour() == s.hour && s.minutes[local.Minute()]
}
