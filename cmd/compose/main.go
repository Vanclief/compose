// Command compose is the command line tool of the compose module.
//
// Install it with
//
//	go install github.com/vanclief/compose/cmd/compose@latest
//
// The reference subcommand renders the API reference page of an OpenAPI
// spec, optionally showing what changed since a base spec, to a file, to
// stdout, or from a local HTTP server that re-renders on every request:
//
//	compose reference -head openapi.yaml -base previous.yaml -out page.html
//	compose reference -head openapi.yaml -base previous.yaml -serve 127.0.0.1:8090
//
// Exit codes are 0 on success, 1 on a runtime error and 2 on a usage error.
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `Usage: compose <command> [flags]

Commands:
  reference   render the API reference page of an OpenAPI spec
  help        show this help

Run 'compose reference -h' for the flags of reference.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command in args and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	switch args[0] {
	case "reference":
		return runReference(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}

	fmt.Fprintf(stderr, "compose: unknown command %q\n\n", args[0])
	fmt.Fprint(stderr, usage)
	return 2
}
