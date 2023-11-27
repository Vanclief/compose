package logger

import (
	"io"
	"os"

	"github.com/carlware/promtail-go"
	"github.com/carlware/promtail-go/client"
	validation "github.com/go-ozzo/ozzo-validation"

	"github.com/rs/zerolog/log"
	"github.com/vanclief/ez"
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

const DEFAULT_TIMEOUT_MS = 500

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

func WithPromtailAndZerolog(params *WithPromtailParams) error {
	const op = "Logger.Setup"

	// Setup PromTail
	writer, err := attachPromtailToWriter(params)
	if err != nil {
		return ez.Wrap(op, err)
	}

	// Setup the logger
	WithZerolog(writer)

	log.Info().
		Str("App", params.App).
		Str("Environment", params.Environment).
		Str("Host", params.PromtailHost).
		Str("Username", params.PromtailUsername).
		Int("Timeout MS", params.PromtailTimeoutMS).
		Bool("Enabled", params.PromtailEnabled).
		Msg("Promtail Config")

	return nil
}

func attachPromtailToWriter(params *WithPromtailParams) (io.Writer, error) {
	const op = "Logger.setupPromtail"

	err := params.Validate()
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	if params.PromtailEnabled {

		opts := []client.Option{}
		opts = append(opts,
			client.WithStaticLabels(map[string]interface{}{
				"env": params.Environment,
				"app": params.App,
			}),
		)

		opts = append(opts,
			client.WithStreamConverter(
				promtail.NewRawStreamConv(params.PromtailLabels, "="),
			),
		)

		opts = append(opts,
			client.WithWriteTimeout(params.PromtailTimeoutMS),
		)

		promtail, err := client.NewSimpleClient(
			params.PromtailHost,
			params.PromtailUsername,
			params.PromtailPassword,
			opts...,
		)
		if err != nil {
			return nil, ez.Wrap(op, err)
		}

		return io.MultiWriter(os.Stdout, promtail), nil
	} else {
		return io.MultiWriter(os.Stdout), nil
	}
}
