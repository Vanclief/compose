package mcp

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"strings"

	"github.com/vanclief/ez"
)

//go:embed templates/reference.html
var referenceFS embed.FS

var referenceTemplate = template.Must(template.ParseFS(referenceFS, "templates/reference.html"))

// referencePage is the view model of the reference page.
type referencePage struct {
	Name         string
	Version      string
	Instructions string
	Tools        []referenceTool
	Modules      []referenceModule
	Terms        []referenceTerm
}

type referenceTool struct {
	Name        string
	Description string
	// Schema is the indented input schema, or empty.
	Schema string
}

type referenceModule struct {
	ID          string
	Name        string
	Description string
	Functions   []referenceFunction
}

type referenceFunction struct {
	ID string
	// Key is the lowercase text the filter matches.
	Key  string
	Name string
	// Operation is "METHOD /path" or "composite".
	Operation string
	// Subtitle is the summary and the return label, as in search results.
	Subtitle    string
	Describe    string
	ReadOnly    bool
	Destructive bool
	Composite   bool
	Deprecated  bool
}

type referenceTerm struct {
	ID      string
	Key     string
	Name    string
	Aliases string
	Explain string
}

// renderReference renders the HTML reference page of the MCP server: the
// instructions, the tools, and for every function and term the same text
// describe_function and explain_term return to the agent.
func renderReference(cat *Catalog, info referenceInfo) ([]byte, error) {
	page := referencePage{
		Name:         info.Name,
		Version:      info.Version,
		Instructions: info.Instructions,
	}

	for _, tool := range info.Tools {
		entry := referenceTool{Name: tool.Name, Description: tool.Description}
		if len(tool.InputSchema) > 0 {
			var schema bytes.Buffer
			err := json.Indent(&schema, tool.InputSchema, "", "  ")
			if err != nil {
				return nil, ez.New(ez.EINTERNAL, "mcp: input schema of tool "+tool.Name+" is not valid JSON", err)
			}
			entry.Schema = schema.String()
		}
		page.Tools = append(page.Tools, entry)
	}

	for _, m := range cat.Modules {
		module := referenceModule{ID: "mod-" + m.Name, Name: m.Name, Description: m.Description}
		for _, f := range m.Functions {
			module.Functions = append(module.Functions, cat.referenceFunction(f))
		}
		page.Modules = append(page.Modules, module)
	}

	for _, t := range cat.Terms {
		page.Terms = append(page.Terms, referenceTerm{
			ID:      "term-" + slug(t.Name),
			Key:     strings.ToLower(strings.Join(append([]string{t.Name}, t.Aliases...), " ")),
			Name:    t.Name,
			Aliases: strings.Join(t.Aliases, ", "),
			Explain: cat.renderExplain([]string{t.Name}),
		})
	}

	var out bytes.Buffer
	err := referenceTemplate.Execute(&out, page)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "mcp: rendering the reference page failed", err)
	}
	return out.Bytes(), nil
}

// referenceFunction is the view of one function.
func (c *Catalog) referenceFunction(f *Function) referenceFunction {
	operation := f.Method + " " + f.Path
	if f.Composite {
		operation = "composite"
	}
	var subtitle []string
	if f.Summary != "" {
		subtitle = append(subtitle, f.Summary)
	}
	returns := c.returnLabel(f)
	if returns != "" {
		subtitle = append(subtitle, "-> "+returns)
	}
	return referenceFunction{
		ID:          "fn-" + f.Module + "-" + strings.TrimPrefix(f.Name, f.Module+"."),
		Key:         strings.ToLower(f.Name + " " + f.Summary),
		Name:        f.Name,
		Operation:   operation,
		Subtitle:    strings.Join(subtitle, "  "),
		Describe:    c.renderDescribe([]string{f.Name}),
		ReadOnly:    f.ReadOnly,
		Destructive: f.Destructive,
		Composite:   f.Composite,
		Deprecated:  f.Deprecated,
	}
}

// slug turns a term name into an anchor fragment: lowercase, with every run
// of characters other than letters, digits, dots, dashes and underscores
// replaced by one dash.
func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		keep := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_'
		if keep {
			b.WriteRune(r)
			continue
		}
		if !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
