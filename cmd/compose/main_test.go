package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{name: "no arguments", args: nil, code: 2, stderr: "Usage: compose"},
		{name: "unknown command", args: []string{"nope"}, code: 2, stderr: `unknown command "nope"`},
		{name: "help", args: []string{"help"}, code: 0, stdout: "Usage: compose"},
		{name: "-h", args: []string{"-h"}, code: 0, stdout: "Usage: compose"},
		{name: "help lists descriptions", args: []string{"help"}, code: 0, stdout: "  descriptions   write the doc comments of Go types"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			require.Equal(t, tt.code, code)
			require.Contains(t, stdout.String(), tt.stdout)
			require.Contains(t, stderr.String(), tt.stderr)
		})
	}
}
