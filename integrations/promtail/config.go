package promtail

import (
	"github.com/vanclief/ez"
)

type Config struct {
	App      string
	Host     string
	Username string
	Labels   string
	Enabled  bool
	Timeout  int
}

const (
	DEFAULT_TIMEOUT_MS       = 5000
	legacyTimeoutThresholdMS = 1000
)

type WithPromtailParams struct {
	App               string
	Environment       string
	PromtailHost      string
	PromtailUsername  string
	PromtailPassword  string
	PromtailLabels    string
	PromtailEnabled   bool
	PromtailTimeoutMS int
}

// EffectiveTimeoutMS returns the timeout the client uses for each batch push.
// The old client applied the timeout to every line, so values below
// legacyTimeoutThresholdMS are taken as leftovers from it and replaced with
// the batch default, as is zero.
func (p WithPromtailParams) EffectiveTimeoutMS() int {
	if p.PromtailTimeoutMS < legacyTimeoutThresholdMS {
		return DEFAULT_TIMEOUT_MS
	}

	return p.PromtailTimeoutMS
}

// Validate only requires the Loki connection settings when Promtail is
// enabled, so apps that don't ship logs can leave them out.
func (p WithPromtailParams) Validate() error {
	if p.App == "" {
		return ez.New(ez.EINVALID, "App is required", nil)
	}

	if p.Environment == "" {
		return ez.New(ez.EINVALID, "Environment is required", nil)
	}

	if !p.PromtailEnabled {
		return nil
	}

	if p.PromtailHost == "" {
		return ez.New(ez.EINVALID, "PromtailHost is required when Promtail is enabled", nil)
	}

	if p.PromtailUsername == "" {
		return ez.New(ez.EINVALID, "PromtailUsername is required when Promtail is enabled", nil)
	}

	return nil
}
