package promtail

import (
	"fmt"
	"io"
	"os"

	"github.com/carlware/promtail-go"
	"github.com/carlware/promtail-go/client"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/vanclief/ez"
)

func WithZerolog(params *WithPromtailParams) error {
	const op = "promtail.WithZeroLog"

	// Setup PromTail
	writer, err := attachToWriter(params)
	if err != nil {
		return ez.Wrap(op, err)
	}

	// Setup the logger
	output := zerolog.ConsoleWriter{Out: writer}

	output.FormatMessage = func(i interface{}) string {
		_, ok := i.(string)
		if ok {
			return fmt.Sprintf("%-50s", i)
		} else {
			return ""
		}
	}

	log.Logger = log.Output(output)

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

func attachToWriter(params *WithPromtailParams) (io.Writer, error) {
	const op = "logging.attachToWriter"

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
