package logger

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

type SetupParams struct {
	App              string
	Environment      string
	PromTailEnabled  bool
	PromTailHost     string
	PromTailUsername string
	PromTailPassword string
	PromTailLabels   string
}

const SECONDS_TIMEOUT_LOGS = 1

func (p *SetupParams) Validate() error {
	const op = "SetupParams.Validate"

	if !p.PromTailEnabled {
		return nil
	}
	if p.App == "" {
		return ez.New(op, ez.EINVALID, "App is required", nil)
	}

	if p.Environment == "" {
		return ez.New(op, ez.EINVALID, "Environment is required", nil)
	}

	if p.PromTailHost == "" {
		return ez.New(op, ez.EINVALID, "PromTailHost is required", nil)
	}

	if p.PromTailUsername == "" {
		return ez.New(op, ez.EINVALID, "PromTailUsername is required", nil)
	}

	if p.PromTailPassword == "" {
		return ez.New(op, ez.EINVALID, "PromTailPassword is required", nil)
	}

	return nil
}

func Setup(params SetupParams) error {
	const op = "Logger.Setup"

	// Setup PromTail
	writer, err := setupPromTail(params)
	if err != nil {
		return ez.Wrap(op, err)
	}

	// Setup the logger
	setupZerolog(writer)

	return nil
}

func setupPromTail(params SetupParams) (io.Writer, error) {
	const op = "Logger.setupPromTail"

	err := params.Validate()
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	if params.PromTailEnabled {

		opts := []client.Option{}
		opts = append(opts,
			client.WithStaticLabels(map[string]interface{}{
				"env": params.Environment,
				"app": params.App,
			}),
		)

		opts = append(opts,
			client.WithStreamConverter(
				promtail.NewRawStreamConv(params.PromTailLabels, "="),
			),
		)

		opts = append(opts,
			client.WithWriteTimeout(SECONDS_TIMEOUT_LOGS),
		)

		promTail, err := client.NewSimpleClient(
			params.PromTailHost,
			params.PromTailUsername,
			params.PromTailPassword,
			opts...,
		)

		if err != nil {
			return nil, ez.Wrap(op, err)
		}

		return io.MultiWriter(os.Stdout, promTail), nil
	} else {
		return io.MultiWriter(os.Stdout), nil
	}
}

func setupZerolog(writer io.Writer) {
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
}
