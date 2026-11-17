package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/descriptions"
)

const descriptionsUsage = `Usage:
  compose descriptions [-out descriptions.json] [dir ...]

Writes the doc comments of the Go types under each directory (default .), and
of their fields, as one JSON object that api.Descriptions loads, so the route
registry's documents describe what the comments describe. Directories named
vendor or testdata, files and directories starting with . or _, and _test.go
files are skipped. Every other file is read whatever its build constraints.
A key that two files describe differently, such as one type in two files
for different platforms or a type of the same name in two main packages, is
left out with a warning on stderr. Make the comments match to describe it.
The output is the same bytes for the same source on every platform, so a CI
job can regenerate it and check it with git diff --exit-code. Without -out it
is written to stdout. Flags go before the directories.

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
	for _, dir := range dirs {
		if strings.HasPrefix(dir, "-") {
			return usageError(fs, stderr, fmt.Sprintf("unexpected argument %q: flags go before the directories", dir))
		}
	}
	if len(dirs) == 0 {
		dirs = []string{"."}
	}

	comments, conflicts, err := descriptions.Read(dirs...)
	if err != nil {
		return fail(stderr, err)
	}
	for _, conflict := range conflicts {
		fmt.Fprintf(stderr, "compose descriptions: %s is described differently in %s and %s: left out, make the comments match to describe it\n",
			conflict.Key, conflict.First, conflict.Second)
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
