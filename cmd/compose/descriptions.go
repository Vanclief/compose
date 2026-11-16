package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/descriptions"
)

const descriptionsUsage = `Usage:
  compose descriptions [-out descriptions.json] [dir ...]

Writes the doc comments of the Go types under each directory (default .), and
of their fields, as one JSON object that api.Descriptions loads, so the route
registry's documents describe what the comments describe. Directories named
vendor or testdata, those starting with . or _, and _test.go files are
skipped. The output is the same bytes for the same source, so a CI job can
regenerate it and check it with git diff --exit-code. Without -out it is
written to stdout.

Flags:
`

// runDescriptions runs the descriptions subcommand and returns the exit code.
func runDescriptions(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("descriptions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, descriptionsUsage)
		fs.PrintDefaults()
	}

	out := fs.String("out", "", "write the JSON to this file instead of stdout")

	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}

	dirs := fs.Args()
	if len(dirs) == 0 {
		dirs = []string{"."}
	}

	comments, err := descriptions.Read(dirs...)
	if err != nil {
		return fail(stderr, err)
	}
	data, err := descriptions.Marshal(comments)
	if err != nil {
		return fail(stderr, err)
	}

	if *out == "" {
		_, err = stdout.Write(data)
		if err != nil {
			return fail(stderr, ez.New(ez.EINTERNAL, "cannot write the descriptions: "+err.Error(), err))
		}
		return 0
	}

	err = os.WriteFile(*out, data, 0o644)
	if err != nil {
		return fail(stderr, ez.New(ez.EINTERNAL, "cannot write "+*out+": "+err.Error(), err))
	}
	return 0
}
