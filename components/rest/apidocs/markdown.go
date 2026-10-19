package apidocs

import (
	"bytes"
	"html/template"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// md renders CommonMark plus GFM tables, strikethrough, task lists and bare
// URL linking. Raw HTML stays disabled (goldmark's default), so a description
// can never inject markup or scripts into the page, and dangerous link schemes
// such as javascript: are dropped.
var md = goldmark.New(
	goldmark.WithExtensions(
		extension.Table,
		extension.Strikethrough,
		extension.TaskList,
		extension.Linkify,
	),
)

// markdown renders an OpenAPI description to safe HTML. Empty input yields an
// empty value so templates can skip the wrapper.
func markdown(src string) template.HTML {
	if strings.TrimSpace(src) == "" {
		return ""
	}

	var buf bytes.Buffer
	err := md.Convert([]byte(src), &buf)
	if err != nil {
		return template.HTML("<p>" + template.HTMLEscapeString(src) + "</p>")
	}

	return template.HTML(buf.String())
}
