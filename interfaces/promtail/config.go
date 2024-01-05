package promtail

import (
	validation "github.com/go-ozzo/ozzo-validation"
	"github.com/vanclief/ez"
)

type Config struct {
	App      string
	Host     string
	Timeout  int
	Username string
	Labels   string
}

const DEFAULT_TIMEOUT_MS = 500

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

func (p WithPromtailParams) Validate() error {
	const op = "WithPromtailParams.Validate"

	err := validation.ValidateStruct(&p,
		validation.Field(&p.App, validation.Required),
		validation.Field(&p.Environment, validation.Required),
		validation.Field(&p.PromtailHost, validation.Required),
		validation.Field(&p.PromtailUsername, validation.Required),
	)
	if err != nil {
		return ez.New(op, ez.EINVALID, err.Error(), nil)
	}

	if p.PromtailTimeoutMS == 0 {
		p.PromtailTimeoutMS = DEFAULT_TIMEOUT_MS
	}

	return nil
}
