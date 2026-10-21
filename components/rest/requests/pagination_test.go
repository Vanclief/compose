package requests

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestKeysetBasedListValidate(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		wantLimit int
		wantErr   bool
	}{
		{name: "negative", limit: -1, wantErr: true},
		{name: "zero applies default", limit: 0, wantLimit: DEFAULT_LIMIT},
		{name: "max", limit: MAX_PAGE_SIZE, wantLimit: MAX_PAGE_SIZE},
		{name: "above max", limit: MAX_PAGE_SIZE + 1, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := &KeysetBasedList{Limit: tc.limit}

			err := request.Validate()
			if tc.wantErr {
				require.Error(t, err)
				require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantLimit, request.Limit)
		})
	}
}

func TestOffsetBasedListValidate(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		offset    int
		wantLimit int
		wantErr   bool
	}{
		{name: "negative limit", limit: -1, wantErr: true},
		{name: "zero applies default", limit: 0, wantLimit: DEFAULT_LIMIT},
		{name: "max", limit: MAX_PAGE_SIZE, wantLimit: MAX_PAGE_SIZE},
		{name: "above max", limit: MAX_PAGE_SIZE + 1, wantErr: true},
		{name: "large limit with large offset", limit: 1000000, offset: 999500, wantErr: true},
		{name: "max with large offset", limit: MAX_PAGE_SIZE, offset: 999500, wantLimit: MAX_PAGE_SIZE},
		{name: "negative offset", limit: 10, offset: -1, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := &OffsetBasedList{Limit: tc.limit, Offset: tc.offset}

			err := request.Validate()
			if tc.wantErr {
				require.Error(t, err)
				require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantLimit, request.Limit)
		})
	}
}
