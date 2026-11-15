// Command modzip checks that the commit at HEAD of a repository can become the
// module zip that go get downloads. It builds that zip with Go's own rules and
// prints every file the rules refuse, with the reason.
//
// Usage: go run . <repository root>
//
// The root must be the top of a git clone. A linked worktree, whose .git is a
// file, is not recognized by zip.CreateFromVCS.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/mod/module"
	"golang.org/x/mod/zip"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: modzip <repository root>")
		os.Exit(2)
	}

	root, err := filepath.Abs(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// The version only names the zip's top directory, so any valid one will do
	m := module.Version{Path: "github.com/vanclief/compose", Version: "v0.0.0-check"}
	err = zip.CreateFromVCS(io.Discard, m, root, "HEAD", "")
	if err == nil {
		return
	}

	var invalid zip.FileErrorList
	if !errors.As(err, &invalid) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "These files keep the commit from becoming a module zip:")
	for _, file := range invalid {
		fmt.Fprintf(os.Stderr, "  %s: %s\n", file.Path, file.Err)
	}
	os.Exit(1)
}
