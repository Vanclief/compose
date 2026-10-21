package mcp

import (
	"bytes"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vanclief/ez"
)

type snapshot struct {
	Name         string           `json:"name"`
	Version      string           `json:"version"`
	Instructions string           `json:"instructions"`
	Tools        []snapshotTool   `json:"tools"`
	Modules      []snapshotModule `json:"modules"`
	Terms        []snapshotTerm   `json:"terms"`
}

type snapshotTool struct {
	Name        string               `json:"name"`
	Description string               `json:"description"`
	Annotations *mcp.ToolAnnotations `json:"annotations"`
	InputSchema json.RawMessage      `json:"input_schema"`
}

type snapshotModule struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Functions   []snapshotFunction `json:"functions"`
}

type snapshotFunction struct {
	Name        string        `json:"name"`
	Summary     string        `json:"summary"`
	Description string        `json:"description"`
	Method      string        `json:"method"`
	Path        string        `json:"path"`
	Args        []snapshotArg `json:"args"`
	Returns     string        `json:"returns"`
	ReadOnly    bool          `json:"read_only"`
	Destructive bool          `json:"destructive"`
	Composite   bool          `json:"composite"`
	Deprecated  bool          `json:"deprecated"`
}

type snapshotArg struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
	Type     string `json:"type"`
}

type snapshotTerm struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Aliases     []string        `json:"aliases"`
	Fields      []snapshotField `json:"fields"`
	AcceptedBy  []string        `json:"accepted_by"`
	ReturnedBy  []string        `json:"returned_by"`
}

type snapshotField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Snapshot returns the agent-facing surface of the server as indented JSON
// in a stable order: identity, instructions, tools, modules with their
// functions, and terms. Projects commit it and compare it in a test to catch
// drift of the API as the agent sees it.
func (s *Server) Snapshot() ([]byte, error) {
	c := s.catalog
	snap := snapshot{
		Name:         s.cfg.Name,
		Version:      s.cfg.Version,
		Instructions: s.instructions,
		Tools:        []snapshotTool{},
		Modules:      []snapshotModule{},
		Terms:        []snapshotTerm{},
	}

	for _, tool := range s.tools {
		snap.Tools = append(snap.Tools, snapshotTool{
			Name:        tool.Name,
			Description: tool.Description,
			Annotations: tool.Annotations,
			InputSchema: inputSchemaJSON(tool),
		})
	}

	for _, m := range c.Modules {
		module := snapshotModule{Name: m.Name, Description: m.Description, Functions: []snapshotFunction{}}
		for _, f := range m.Functions {
			function := snapshotFunction{
				Name:        f.Name,
				Summary:     f.Summary,
				Description: f.Description,
				Method:      f.Method,
				Path:        f.Path,
				Args:        []snapshotArg{},
				Returns:     c.returnLabel(f),
				ReadOnly:    f.ReadOnly,
				Destructive: f.Destructive,
				Composite:   f.Composite,
				Deprecated:  f.Deprecated,
			}
			for _, a := range f.Args {
				function.Args = append(function.Args, snapshotArg{Name: a.Name, In: a.In, Required: a.Required, Type: c.typeName(a.Schema)})
			}
			module.Functions = append(module.Functions, function)
		}
		snap.Modules = append(snap.Modules, module)
	}

	for _, t := range c.Terms {
		term := snapshotTerm{
			Name:        t.Name,
			Description: t.Description,
			Aliases:     nonNil(t.Aliases),
			Fields:      []snapshotField{},
			AcceptedBy:  nonNil(t.AcceptedBy),
			ReturnedBy:  nonNil(t.ReturnedBy),
		}
		if t.Schema != nil {
			for _, key := range t.Schema.Properties.Keys {
				term.Fields = append(term.Fields, snapshotField{Name: key, Type: c.typeName(t.Schema.Properties.Map[key])})
			}
		}
		snap.Terms = append(snap.Terms, term)
	}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	err := encoder.Encode(snap)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "mcp: cannot encode the snapshot: "+err.Error(), err)
	}
	return buf.Bytes(), nil
}

// nonNil returns list, or an empty list when it is nil, so the snapshot
// prints [] instead of null.
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
