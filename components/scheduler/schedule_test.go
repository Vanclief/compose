package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// selectedMinutes lists the minutes set in s, in ascending order.
func selectedMinutes(s Schedule) []int {
	var out []int
	for m, selected := range s.minutes {
		if selected {
			out = append(out, m)
		}
	}
	return out
}

// minuteRange returns start, start+step, ... up to 59.
func minuteRange(step int) []int {
	var out []int
	for m := 0; m < 60; m += step {
		out = append(out, m)
	}
	return out
}

func loadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func TestEveryValid(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5, 6, 10, 12, 15, 20, 30, 60} {
		s := Every(time.Duration(n) * time.Minute)
		require.NoError(t, s.validate(), "Every(%dm)", n)
		assert.Equal(t, minuteRange(n), selectedMinutes(s), "Every(%dm)", n)
		assert.Nil(t, s.loc)
	}

	assert.Equal(t, []int{0, 15, 30, 45}, selectedMinutes(Every(15*time.Minute)))
	assert.Equal(t, []int{0}, selectedMinutes(Every(time.Hour)))
	assert.Equal(t, Hourly(0), Every(time.Hour))
}

func TestEveryInvalid(t *testing.T) {
	cases := []time.Duration{
		0,
		-5 * time.Minute,
		90 * time.Second,
		7 * time.Minute,
		90 * time.Minute,
		2 * time.Hour,
		time.Nanosecond,
	}

	for _, d := range cases {
		s := Every(d)
		err := s.validate()
		require.Error(t, err, "Every(%s)", d)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), "Every(%s)", d)
		assert.Empty(t, selectedMinutes(s), "Every(%s)", d)
	}
}

func TestHourlyValid(t *testing.T) {
	cases := []struct {
		name    string
		minutes []int
		want    []int
	}{
		{name: "single zero", minutes: []int{0}, want: []int{0}},
		{name: "single last", minutes: []int{59}, want: []int{59}},
		{name: "several", minutes: []int{0, 30}, want: []int{0, 30}},
		{name: "unordered", minutes: []int{45, 5, 20}, want: []int{5, 20, 45}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Hourly(tc.minutes...)
			require.NoError(t, s.validate())
			assert.Equal(t, tc.want, selectedMinutes(s))
			assert.Nil(t, s.loc)
		})
	}
}

func TestHourlyInvalid(t *testing.T) {
	cases := []struct {
		name    string
		minutes []int
	}{
		{name: "empty", minutes: nil},
		{name: "negative", minutes: []int{-1}},
		{name: "sixty", minutes: []int{60}},
		{name: "out of range among valid", minutes: []int{0, 15, 75}},
		{name: "duplicate", minutes: []int{10, 20, 10}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Hourly(tc.minutes...)
			err := s.validate()
			require.Error(t, err)
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			assert.Empty(t, selectedMinutes(s))
		})
	}
}

func TestHourlyCopiesInput(t *testing.T) {
	minutes := []int{5, 10}
	s := Hourly(minutes...)
	minutes[0] = 20

	assert.Equal(t, []int{5, 10}, selectedMinutes(s))
	assert.True(t, s.matches(time.Date(2026, 9, 24, 10, 5, 0, 0, time.Local)))
	assert.False(t, s.matches(time.Date(2026, 9, 24, 10, 20, 0, 0, time.Local)))
}

func TestDailyValid(t *testing.T) {
	mx := loadLocation(t, "America/Mexico_City")

	s := Daily(9, 30, mx)
	require.NoError(t, s.validate())
	assert.Equal(t, []int{30}, selectedMinutes(s))
	assert.Equal(t, 9, s.hour)
	assert.Equal(t, mx, s.loc)

	require.NoError(t, Daily(0, 0, time.UTC).validate())
	require.NoError(t, Daily(23, 59, time.UTC).validate())
}

func TestDailyInvalid(t *testing.T) {
	cases := []struct {
		name   string
		hour   int
		minute int
		loc    *time.Location
	}{
		{name: "hour 24", hour: 24, minute: 0, loc: time.UTC},
		{name: "negative hour", hour: -1, minute: 0, loc: time.UTC},
		{name: "minute 60", hour: 9, minute: 60, loc: time.UTC},
		{name: "negative minute", hour: 9, minute: -1, loc: time.UTC},
		{name: "nil location", hour: 9, minute: 0, loc: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Daily(tc.hour, tc.minute, tc.loc)
			err := s.validate()
			require.Error(t, err)
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			assert.Empty(t, selectedMinutes(s))
			assert.Nil(t, s.loc)
		})
	}
}

func TestScheduleZeroValue(t *testing.T) {
	var s Schedule
	err := s.validate()
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))

	for m := 0; m < 60; m++ {
		assert.False(t, s.matches(time.Date(2026, 9, 24, 10, m, 0, 0, time.Local)))
	}
}

func TestScheduleInvalidMatchesNothing(t *testing.T) {
	invalid := []Schedule{Every(7 * time.Minute), Hourly(), Daily(24, 0, time.UTC)}

	for _, s := range invalid {
		for m := 0; m < 60; m++ {
			assert.False(t, s.matches(time.Date(2026, 9, 24, 0, m, 0, 0, time.UTC)))
		}
	}
}

func TestMatchesMinuteSet(t *testing.T) {
	cases := []struct {
		name string
		when Schedule
		at   time.Time
		want bool
	}{
		{name: "every 15 at :15", when: Every(15 * time.Minute), at: time.Date(2026, 9, 24, 10, 15, 0, 0, time.Local), want: true},
		{name: "every 15 at :00", when: Every(15 * time.Minute), at: time.Date(2026, 9, 24, 10, 0, 0, 0, time.Local), want: true},
		{name: "every 15 at :16", when: Every(15 * time.Minute), at: time.Date(2026, 9, 24, 10, 16, 0, 0, time.Local), want: false},
		{name: "every 15 ignores seconds", when: Every(15 * time.Minute), at: time.Date(2026, 9, 24, 10, 45, 30, 0, time.Local), want: true},
		{name: "every minute", when: Every(time.Minute), at: time.Date(2026, 9, 24, 3, 7, 0, 0, time.Local), want: true},
		{name: "every hour at :00", when: Every(time.Hour), at: time.Date(2026, 9, 24, 23, 0, 0, 0, time.Local), want: true},
		{name: "every hour at :30", when: Every(time.Hour), at: time.Date(2026, 9, 24, 23, 30, 0, 0, time.Local), want: false},
		{name: "hourly any hour", when: Hourly(5, 50), at: time.Date(2026, 9, 24, 17, 50, 0, 0, time.Local), want: true},
		{name: "hourly ignores seconds", when: Hourly(5, 50), at: time.Date(2026, 9, 24, 17, 5, 30, 0, time.Local), want: true},
		{name: "hourly unselected", when: Hourly(5, 50), at: time.Date(2026, 9, 24, 17, 6, 0, 0, time.Local), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.when.matches(tc.at))
		})
	}
}

func TestMatchesMinuteSetUsesLocalTime(t *testing.T) {
	s := Hourly(15)
	at := time.Date(2026, 9, 24, 10, 15, 0, 0, time.Local)

	// The same instant expressed in another zone still matches, because the
	// minute is read from t.Local().
	assert.True(t, s.matches(at.UTC()))
}

func TestMatchesDaily(t *testing.T) {
	mx := loadLocation(t, "America/Mexico_City")
	tj := loadLocation(t, "America/Tijuana")

	s := Daily(9, 0, mx)
	assert.True(t, s.matches(time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)))
	assert.False(t, s.matches(time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC)))
	assert.True(t, s.matches(time.Date(2026, 9, 24, 15, 0, 30, 0, time.UTC)), "seconds are ignored")
	assert.False(t, s.matches(time.Date(2026, 9, 24, 15, 1, 0, 0, time.UTC)))
	assert.True(t, s.matches(time.Date(2026, 9, 24, 9, 0, 0, 0, mx)))

	// The same wall time in another zone is a different instant.
	assert.False(t, Daily(9, 0, tj).matches(time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)))
}

func TestMatchesDailyNoDSTMexicoCity(t *testing.T) {
	mx := loadLocation(t, "America/Mexico_City")

	at := time.Date(2026, 3, 8, 2, 30, 0, 0, mx)
	require.Equal(t, 2, at.Hour())
	require.Equal(t, 30, at.Minute())
	assert.True(t, Daily(2, 30, mx).matches(at))
}

func TestMatchesDailySpringForwardTijuana(t *testing.T) {
	tj := loadLocation(t, "America/Tijuana")

	// 02:30 does not exist on 2026-03-08 in Tijuana. time.Date normalizes it
	// to some other wall time (Go documents the choice as not guaranteed), so
	// only assert that it is no longer 02:30 and does not match.
	s := Daily(2, 30, tj)
	nonexistent := time.Date(2026, 3, 8, 2, 30, 0, 0, tj)
	require.False(t, nonexistent.Hour() == 2 && nonexistent.Minute() == 30)
	assert.False(t, s.matches(nonexistent))

	start := time.Date(2026, 3, 8, 0, 0, 0, 0, tj)
	end := time.Date(2026, 3, 9, 0, 0, 0, 0, tj)
	for at := start; at.Before(end); at = at.Add(time.Minute) {
		assert.False(t, s.matches(at), "unexpected match at %s", at.UTC())
	}

	// The following day it matches again.
	assert.True(t, s.matches(time.Date(2026, 3, 9, 2, 30, 0, 0, tj)))
}

func TestMatchesDailyFallBackTijuana(t *testing.T) {
	tj := loadLocation(t, "America/Tijuana")

	s := Daily(1, 0, tj)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, tj)
	end := time.Date(2026, 11, 2, 0, 0, 0, 0, tj)

	var hits []time.Time
	for at := start; at.Before(end); at = at.Add(time.Minute) {
		if s.matches(at) {
			hits = append(hits, at.UTC())
		}
	}

	require.Len(t, hits, 2)
	assert.Equal(t, time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC), hits[0])
	assert.Equal(t, time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), hits[1])
}
