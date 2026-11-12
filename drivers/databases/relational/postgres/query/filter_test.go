package query

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestTimeRangeFiltersUsesBoundsAsGiven(t *testing.T) {
	cases := []struct {
		name string
		from string
		to   string
	}{
		{"midnight UTC", "2026-03-10T00:00:00Z", "2026-03-12T00:00:00Z"},
		{"local midnight with an offset", "2026-03-10T00:00:00-06:00", "2026-03-12T00:00:00-06:00"},
		{"local midnight written in UTC", "2026-03-10T06:00:00Z", "2026-03-12T06:00:00Z"},
		{"time of day", "2026-03-10T09:15:30Z", "2026-03-12T17:45:10Z"},
		{"last second of a day", "2026-03-10T00:00:00Z", "2026-03-12T23:59:59Z"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, err := time.Parse(time.RFC3339, tc.from)
			require.NoError(t, err)
			to, err := time.Parse(time.RFC3339, tc.to)
			require.NoError(t, err)

			filters, err := TimeRangeFilters("created_at", tc.from, tc.to)
			require.NoError(t, err)
			require.Equal(t, []Filter{
				{Field: "created_at", Value: from.Unix(), Comparison: ">="},
				{Field: "created_at", Value: to.Unix(), Comparison: "<="},
			}, filters)
		})
	}
}

func TestTimeRangeFiltersDropsFractionOfSecond(t *testing.T) {
	filters, err := TimeRangeFilters("created_at", "2026-03-10T09:15:30.9Z", "2026-03-12T17:45:10.9Z")
	require.NoError(t, err)
	require.Equal(t, []Filter{
		{Field: "created_at", Value: time.Date(2026, 3, 10, 9, 15, 30, 0, time.UTC).Unix(), Comparison: ">="},
		{Field: "created_at", Value: time.Date(2026, 3, 12, 17, 45, 10, 0, time.UTC).Unix(), Comparison: "<="},
	}, filters)
}

func TestTimeRangeFiltersEmptyBounds(t *testing.T) {
	filters, err := TimeRangeFilters("created_at", "", "")
	require.NoError(t, err)
	require.Empty(t, filters)

	filters, err = TimeRangeFilters("created_at", "2026-03-10T00:00:00Z", "")
	require.NoError(t, err)
	require.Equal(t, []Filter{
		{Field: "created_at", Value: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC).Unix(), Comparison: ">="},
	}, filters)

	filters, err = TimeRangeFilters("created_at", "", "2026-03-12T00:00:00Z")
	require.NoError(t, err)
	require.Equal(t, []Filter{
		{Field: "created_at", Value: time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC).Unix(), Comparison: "<="},
	}, filters)
}

func TestTimeRangeFiltersRefusesWhatIsNotRFC3339(t *testing.T) {
	values := []string{"2026-03-10", "2026-03-10 00:00:00", "10/03/2026", "1773100800", "tomorrow"}

	for _, value := range values {
		_, err := TimeRangeFilters("created_at", value, "")
		require.Error(t, err, value)
		require.Equal(t, ez.EINVALID, ez.ErrorCode(err), value)
		require.Equal(t, "invalid from_date format", ez.ErrorMessage(err), value)

		_, err = TimeRangeFilters("created_at", "", value)
		require.Error(t, err, value)
		require.Equal(t, ez.EINVALID, ez.ErrorCode(err), value)
		require.Equal(t, "invalid to_date format", ez.ErrorMessage(err), value)
	}
}
