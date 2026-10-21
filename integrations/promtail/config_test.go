package promtail

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vanclief/ez"
)

func TestValidateOnlyRequiresLokiSettingsWhenEnabled(t *testing.T) {
	tests := []struct {
		name    string
		params  WithPromtailParams
		wantErr bool
	}{
		{
			name:   "disabled without Loki settings",
			params: WithPromtailParams{App: "api", Environment: "local"},
		},
		{
			name:    "missing app",
			params:  WithPromtailParams{Environment: "local"},
			wantErr: true,
		},
		{
			name:    "enabled without host",
			params:  WithPromtailParams{App: "api", Environment: "production", PromtailEnabled: true, PromtailUsername: "api"},
			wantErr: true,
		},
		{
			name:    "enabled without username",
			params:  WithPromtailParams{App: "api", Environment: "production", PromtailEnabled: true, PromtailHost: "http://loki"},
			wantErr: true,
		},
		{
			name:   "enabled with Loki settings",
			params: WithPromtailParams{App: "api", Environment: "production", PromtailEnabled: true, PromtailHost: "http://loki", PromtailUsername: "api"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.params.Validate()
			if !tt.wantErr {
				assert.NoError(t, err)
				return
			}

			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		})
	}
}

func TestEffectiveTimeoutMSReplacesLegacyValues(t *testing.T) {
	tests := []struct {
		timeoutMS int
		want      int
	}{
		{timeoutMS: 0, want: DEFAULT_TIMEOUT_MS},
		{timeoutMS: 250, want: DEFAULT_TIMEOUT_MS},
		{timeoutMS: 999, want: DEFAULT_TIMEOUT_MS},
		{timeoutMS: 1000, want: 1000},
		{timeoutMS: 2000, want: 2000},
	}

	for _, tt := range tests {
		params := WithPromtailParams{PromtailTimeoutMS: tt.timeoutMS}
		assert.Equal(t, tt.want, params.EffectiveTimeoutMS(), "timeout %dms", tt.timeoutMS)
	}
}
