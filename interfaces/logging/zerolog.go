package logging

import (
	"fmt"
	"io"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func WithZerolog(writer io.Writer) {
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
