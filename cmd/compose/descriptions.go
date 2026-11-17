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
registry's documents describe what the comments describe. Below each
directory given, directories named vendor or testdata or starting with . or _
are skipped, and so are files starting with . or _, _test.go files and files
whose build constraint is exactly ignore. A directory given is always read.
Every other file is read once, whatever its build constraints.

A type declared more than once in a package, such as in two files for
different platforms, is not described, nor are its fields, whatever the
comments say. A warning on stderr names it and two of its files.

A type of a package main is written under its import path, and also under
main when it is the only main package read that declares types. With two or
more, nothing is written under main and a warning names them. Run the
command on the directories of one command at a time to describe the types of
its package main.

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

	result, err := descriptions.Read(dirs...)
	if err != nil {
		return fail(stderr, err)
	}
	for _, duplicate := range result.Duplicates {
		fmt.Fprintf(stderr, "compose descriptions: %s is declared in %s and %s: left out, a type declared more than once in a package is not described\n",
			duplicate.Key, duplicate.First, duplicate.Second)
	}
	if len(result.MainPackages) > 0 {
		fmt.Fprintf(stderr, "compose descriptions: %d main packages declare types (%s), so no type is described under main: run the command on the directories of one command at a time to describe the types of its package main\n",
			len(result.MainPackages), strings.Join(result.MainPackages, ", "))
	}
	data, err := descriptions.Marshal(result.Descriptions)
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
