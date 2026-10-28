package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/vanclief/compose/components/rest/reference"
	"github.com/vanclief/ez"
)

const referenceUsage = `Usage:
  compose reference -head head.yaml [-base base.yaml] [-title T] [-server URL] [-out page.html]
  compose reference -head head.yaml [-base base.yaml] [-title T] [-server URL] -serve 127.0.0.1:8090

Renders the API reference page of an OpenAPI spec (YAML or JSON) with its CSS
and JavaScript inlined. With -base the page shows what changed since that spec.
Without -out or -serve the page is written to stdout. With -serve every request
re-reads the specs and renders the page again.

Flags:
`

// runReference runs the reference subcommand and returns the exit code.
func runReference(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reference", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, referenceUsage)
		fs.PrintDefaults()
	}

	headPath := fs.String("head", "", "path to the spec to render (required, YAML or JSON)")
	basePath := fs.String("base", "", "path to an older spec; the page shows what changed since it")
	title := fs.String("title", "", "page title (defaults to info.title)")
	server := fs.String("server", "", "base URL for curl samples and the try-it client")
	out := fs.String("out", "", "write the page to this file instead of stdout")
	addr := fs.String("serve", "", "serve the page on this address, re-rendering on every request")

	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}

	if fs.NArg() > 0 {
		return usageError(fs, stderr, fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}
	if *headPath == "" {
		return usageError(fs, stderr, "-head is required")
	}
	if *out != "" && *addr != "" {
		return usageError(fs, stderr, "-out and -serve cannot be used together")
	}

	if *addr != "" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		handler := referenceHandler(*headPath, *basePath, *title, *server, stderr)
		return serve(ctx, *addr, handler, *headPath, *basePath, stderr)
	}

	html, err := render(*headPath, *basePath, *title, *server)
	if err != nil {
		return fail(stderr, err)
	}

	if *out == "" {
		_, err = stdout.Write(html)
		if err != nil {
			return fail(stderr, ez.New(ez.EINTERNAL, "cannot write the page: "+err.Error(), err))
		}
		return 0
	}

	err = os.WriteFile(*out, html, 0o644)
	if err != nil {
		return fail(stderr, ez.New(ez.EINTERNAL, "cannot write "+*out+": "+err.Error(), err))
	}
	return 0
}

func usageError(fs *flag.FlagSet, stderr io.Writer, message string) int {
	fmt.Fprintf(stderr, "compose: %s\n\n", message)
	fs.Usage()
	return 2
}

// fail prints the message of err and returns the runtime error exit code.
// Every error reaching it is an ez error whose message names the file or
// address involved, so the user sees what failed rather than an op chain.
func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "compose: %s\n", ez.ErrorMessage(err))
	return 1
}

// render reads the head spec and, when basePath is set, the base spec, and
// renders the self-contained reference page.
func render(headPath, basePath, title, server string) ([]byte, error) {
	head, err := os.ReadFile(headPath)
	if err != nil {
		return nil, ez.New(ez.EINVALID, "cannot read "+headPath+": "+err.Error(), err)
	}

	var base []byte
	if basePath != "" {
		base, err = os.ReadFile(basePath)
		if err != nil {
			return nil, ez.New(ez.EINVALID, "cannot read "+basePath+": "+err.Error(), err)
		}
	}

	page, err := reference.New(head, reference.Options{Title: title, ServerURL: server, Base: base})
	if err != nil {
		// The library names the base document in its message; the head is
		// everything else.
		return nil, ez.New(ez.ErrorCode(err), headPath+": "+ez.ErrorMessage(err), err)
	}
	return page.HTML, nil
}

// referenceHandler re-reads the specs and renders the page on every GET of
// the root path, so edits show up on reload; any other request is 404, so a
// browser's favicon request does not render the page a second time.
// Failures are answered 500 with the error as plain text and logged to
// stderr.
func referenceHandler(headPath, basePath, title, server string, stderr io.Writer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		html, err := render(headPath, basePath, title, server)
		if err != nil {
			message := ez.ErrorMessage(err)
			fmt.Fprintf(stderr, "compose: %s\n", message)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, message)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(html)
	})
	return mux
}

// serve runs handler on addr until ctx is done, then shuts the server down,
// waiting up to five seconds for in-flight responses.
func serve(ctx context.Context, addr string, handler http.Handler, headPath, basePath string, stderr io.Writer) int {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fail(stderr, ez.New(ez.EUNAVAILABLE, "cannot listen on "+addr+": "+err.Error(), err))
	}

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	specs := "head: " + headPath
	if basePath != "" {
		specs += ", base: " + basePath
	}
	fmt.Fprintf(stderr, "serving %s (%s)\n", serveURL(listener.Addr()), specs)

	served := make(chan error, 1)
	go func() {
		served <- server.Serve(listener)
	}()

	select {
	case err = <-served:
		return fail(stderr, ez.New(ez.EINTERNAL, "server stopped: "+err.Error(), err))
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = server.Shutdown(shutdownCtx)
	if err != nil {
		return fail(stderr, ez.New(ez.EINTERNAL, "shutdown: "+err.Error(), err))
	}
	return 0
}

// serveURL is the URL to open for a listener: a wildcard address such as
// ":8090" is shown as localhost, since browsers refuse to open [::] or
// 0.0.0.0.
func serveURL(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if ok && tcp.IP.IsUnspecified() {
		return "http://localhost:" + strconv.Itoa(tcp.Port)
	}
	return "http://" + addr.String()
}
