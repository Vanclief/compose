package generator

import (
	"go/ast"
	"reflect"
	"strconv"
	"strings"
)

// commentText is a doc comment as a description: its text without the "Deprecated:" paragraph
// (the schema says deprecated) and without TODO, FIXME and nolint lines, which are notes for
// maintainers.
func commentText(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}

	var kept []string
	deprecated := false
	for _, line := range strings.Split(group.Text(), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			deprecated = false
		}
		if strings.HasPrefix(trimmed, "Deprecated: ") {
			deprecated = true
		}
		if deprecated || isMaintainerNote(trimmed) {
			continue
		}
		kept = append(kept, line)
	}

	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func isMaintainerNote(line string) bool {
	for _, prefix := range []string{"TODO", "FIXME", "nolint"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}

	return false
}

// fieldDoc describes a struct field: its doc comment, then its line comment.
func fieldDoc(field *ast.Field) string {
	var parts []string
	for _, group := range []*ast.CommentGroup{field.Doc, field.Comment} {
		text := commentText(group)
		if text != "" {
			parts = append(parts, text)
		}
	}

	return strings.Join(parts, "\n\n")
}

// constDoc describes an enum constant: its line comment, or a doc comment that starts with
// its name the Go way ("SmallKind fits in a pocket."). Other doc comments above a constant
// usually head a group of them ("// Right hand"), so they describe none.
func constDoc(spec *ast.ValueSpec, name string) string {
	line := commentText(spec.Comment)
	if line != "" {
		return line
	}

	doc := commentText(spec.Doc)
	if doc == name || strings.HasPrefix(doc, name+" ") {
		return doc
	}

	return ""
}

// describedSchema returns schema with description set, a copy so shared schemas stay
// untouched. A $ref is wrapped in allOf first: OpenAPI 3.0 ignores the siblings of a $ref.
func describedSchema(schema map[string]any, description string) map[string]any {
	if description == "" {
		return schema
	}

	described := siblingSafe(schema)
	described["description"] = description

	return described
}

// isBunBaseModel reports an embedded github.com/uptrace/bun BaseModel.
func isBunBaseModel(expr ast.Expr, imports map[string]string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel == nil || selector.Sel.Name != "BaseModel" {
		return false
	}
	pkgIdent, ok := selector.X.(*ast.Ident)
	if !ok {
		return false
	}

	return imports[pkgIdent.Name] == "github.com/uptrace/bun"
}

// bunTable reads the table name off a BaseModel field tag, `bun:"table:cards,alias:c"`.
func bunTable(field *ast.Field) string {
	if field.Tag == nil {
		return ""
	}
	raw, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return ""
	}

	for _, option := range strings.Split(reflect.StructTag(raw).Get("bun"), ",") {
		name, ok := strings.CutPrefix(strings.TrimSpace(option), "table:")
		if ok {
			return strings.TrimSpace(name)
		}
	}

	return ""
}
