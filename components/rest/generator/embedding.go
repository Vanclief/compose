package generator

import (
	"go/ast"
	"go/parser"
	"go/types"
	"slices"
	"strconv"
	"strings"
)

// promotedField is a field encoding/json writes for a struct: one of its own, or one an
// embedded struct promotes.
type promotedField struct {
	name    string
	field   *ast.Field
	imports map[string]string
	pkgPath string
	// depth is 0 for the struct's own fields and one more per embedded struct above the field.
	depth int
	// tagged is a name given by a json tag.
	tagged bool
	// top numbers the struct's own field name the field is, or the embedded field it comes
	// from.
	top int
	// optional is a field reached through an embedded pointer, which is left out when the
	// pointer is nil.
	optional bool
	// index is the field's position in its struct, after those of the embedded fields above it:
	// the order encoding/json writes fields in.
	index []int
	// generic is the embedded instantiation of a generic type the field is declared in, as
	// written (genericEmbed), "" for any other field.
	generic string
}

// genericEmbed is an embedded instantiation of a generic type, an alias of one or a type declared
// from one, which the walk expands for its fields' names, not their types (genericStruct).
type genericEmbed struct {
	// name is the embedded type as written, Box[int], BoxInt or D.
	name    string
	expr    ast.Expr
	imports map[string]string
	pkgPath string
	decl    sourceStruct
	// optional reports an embedding through a pointer, its own or one above it.
	optional bool
}

// opaqueEmbed is an embedded struct the walk does not expand.
type opaqueEmbed struct {
	expr    ast.Expr
	imports map[string]string
	pkgPath string
}

// embedding is what walkEmbedding finds in a struct.
type embedding struct {
	fields []promotedField
	// opaque are the embedded structs whose fields the generator does not read: described in
	// Config.TypeSchemas without a marshaler, well-known, standard library, or unresolved
	// (embeddedStructDecl).
	opaque []opaqueEmbed
	// generics are the embedded instantiations of generic types, aliases of and types declared
	// from them included: the walk has their fields' names, not their types (genericStruct).
	generics []genericEmbed
	// pointer reports a struct's own embedded pointer to a struct the walk expanded.
	pointer bool
	// marshals reports an expanded struct whose component is not its fields (marshalsItself):
	// encoding/json writes it as something else when it stands alone, or the generator cannot
	// tell, so that component cannot be an allOf member of a struct that only gets its fields.
	marshals bool
}

// embeddedStruct is a struct the walk expands, with where it was found.
type embeddedStruct struct {
	decl     sourceStruct
	depth    int
	top      int
	optional bool
	index    []int
	generic  genericEmbed
}

// mergedStructSchema is the schema of a struct whose embedded structs cannot be allOf members:
// one of their names collides with another name of the struct, a struct is embedded through
// a pointer, or an embedded struct marshals itself when it stands alone (marshalsItself) and
// so has a component that is not its fields. It follows encoding/json: the shallowest field of
// a name wins, a tie at the same depth goes to the one field with a json tag and otherwise drops
// the name, and the fields reached through an embedded pointer are not required, as a nil
// pointer leaves them out. ok is false for any other struct, whose allOf schema already says
// what Go writes, and for one into which an embedded type promotes MarshalJSON or MarshalText,
// which Go writes as that type alone, or for which the generator cannot tell whether one does
// (promotedMarshaler). Such a struct is refused before it gets here: a named one when its
// component is built (customMarshalReason), an anonymous one in schemaForExpr. Only a named one
// that Config.TypeSchemas extends reaches here, and it keeps its allOf.
//
// The walk has the names of an embedded instantiation's fields, not their types (genericStruct).
// The names do not depend on the type arguments, so they take part in resolving the struct's
// names, and the instantiation, an alias of one or a type declared from one is the allOf member
// referencing its description beside the merged properties, its fields left out of them. That
// member says what Go writes unless the instantiation is embedded through a pointer, shares a
// field name, has a marshal method a tie leaves unpromoted or embeds a type in its declaration
// (genericEmbedReason): then the struct is refused, reason says which, and the caller records it.
//
// The embedded structs it does not read (embeddedStructDecl) stay allOf members beside the
// merged properties; their names are not resolved against the others, and their properties
// stay required behind an embedded pointer. It reads database/sql's Null types as the structs
// they are, a described type with a marshaler from source, and a standard-library or well-known
// struct with a marshal method as a struct without fields.
func (b *schemaBuilder) mergedStructSchema(st *ast.StructType, imports map[string]string, pkgPath string) (map[string]any, bool, string) {
	walk := b.walkEmbedding(st, imports, pkgPath)
	collision := collides(walk.fields)
	if !walk.pointer && !walk.marshals && !collision {
		return nil, false, ""
	}
	if b.promotedMarshaler(st, imports, pkgPath, false) != "" {
		return nil, false, ""
	}
	for _, generic := range walk.generics {
		reason := b.genericEmbedReason(generic, walk.fields)
		if reason != "" {
			return nil, false, reason
		}
	}

	props := map[string]any{}
	var required []string
	for _, promoted := range dominantFields(walk.fields) {
		// A generic type's fields are its description's, an allOf member below.
		if promoted.generic != "" {
			continue
		}
		schema := b.fieldSchema(promoted.field, promoted.imports, promoted.pkgPath)
		if hasDeprecatedParagraph(promoted.field) {
			schema = deprecatedSchema(schema)
		}
		if b.docComments {
			schema = describedSchema(schema, fieldDoc(promoted.field))
		}
		props[promoted.name] = schema

		if !promoted.optional && !jsonFieldOmitsEmpty(promoted.field) && !b.isNilableFieldType(promoted.field.Type, promoted.imports, promoted.pkgPath, 0) {
			required = append(required, promoted.name)
		}
	}
	b.markDerived(required)

	object := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		object["required"] = required
	}

	schema := object
	if len(walk.opaque) > 0 || len(walk.generics) > 0 {
		var allOf []any
		for _, opaque := range walk.opaque {
			allOf = append(allOf, b.schemaForExpr(opaque.expr, opaque.imports, opaque.pkgPath))
		}
		for _, generic := range walk.generics {
			allOf = append(allOf, b.schemaForExpr(generic.expr, generic.imports, generic.pkgPath))
		}
		schema = map[string]any{
			"allOf": append(allOf, object),
		}
	}

	for _, field := range st.Fields.List {
		if field != nil && len(field.Names) == 0 && isBunBaseModel(field.Type, imports) {
			table := bunTable(field)
			if table != "" {
				schema["x-table"] = table
			}
		}
	}

	return schema, true, ""
}

// walkEmbedding collects the fields encoding/json finds in st, following its typeFields: breadth
// first, one depth of embedding at a time, an embedded struct without a json tag name promotes
// its fields and any other embedded type is a field named after its type. Each embedded struct
// type is expanded once in the whole walk, at the shallowest depth it is embedded at, which ends
// the walk of a recursive embedding (Node{*Node}). The struct walked is not marked, unlike in
// typeFields, so Node{*Node} expands Node once at depth 1; those fields lose to Node's own at
// depth 0, which have the same names. A struct type embedded more than once at one depth has
// each of its fields listed twice, so that they tie and cancel out.
func (b *schemaBuilder) walkEmbedding(st *ast.StructType, imports map[string]string, pkgPath string) *embedding {
	walk := &embedding{}
	visited := map[typeRef]bool{}
	next := []embeddedStruct{{decl: sourceStruct{st: st, imports: imports, pkgPath: pkgPath}}}
	nextCount := map[typeRef]int{}
	own := 0

	for len(next) > 0 {
		current := next
		count := nextCount
		next = nil
		nextCount = map[typeRef]int{}

		for _, entry := range current {
			if entry.depth > 0 {
				if visited[entry.decl.ref] {
					continue
				}
				visited[entry.decl.ref] = true
			}
			if entry.generic.name != "" {
				walk.generics = append(walk.generics, entry.generic)
			}

			for i, field := range entry.decl.st.Fields.List {
				if field == nil {
					continue
				}
				tagName, skipped := jsonTagName(field)
				if skipped {
					continue
				}
				index := append(slices.Clone(entry.index), i)
				top := entry.top
				if entry.depth == 0 {
					top = own
					own++
				}

				var names []string
				if len(field.Names) == 0 {
					// bun.BaseModel marshals nothing.
					if isBunBaseModel(field.Type, entry.decl.imports) {
						continue
					}
					isStruct := b.isStructType(derefExpr(field.Type), entry.decl.imports, entry.decl.pkgPath, 0)
					if isStruct && tagName == "" {
						decl, ok := b.embeddedStructDecl(derefExpr(field.Type), entry.decl.imports, entry.decl.pkgPath)
						if !ok {
							walk.opaque = append(walk.opaque, opaqueEmbed{expr: derefExpr(field.Type), imports: entry.decl.imports, pkgPath: entry.decl.pkgPath})
							continue
						}
						_, isPointer := field.Type.(*ast.StarExpr)
						if isPointer && entry.depth == 0 {
							walk.pointer = true
						}
						var generic genericEmbed
						if decl.generic != (typeRef{}) {
							generic = genericEmbed{
								name:     types.ExprString(derefExpr(field.Type)),
								expr:     derefExpr(field.Type),
								imports:  entry.decl.imports,
								pkgPath:  entry.decl.pkgPath,
								decl:     decl,
								optional: entry.optional || isPointer,
							}
						}
						nextCount[decl.ref]++
						if nextCount[decl.ref] == 1 {
							// Each struct type is asked once, until one marshals itself.
							walk.marshals = walk.marshals || (!visited[decl.ref] && b.marshalsItself(decl))
							next = append(next, embeddedStruct{
								decl:     decl,
								depth:    entry.depth + 1,
								top:      top,
								optional: entry.optional || isPointer,
								index:    index,
								generic:  generic,
							})
						}
						continue
					}
					typeName := embeddedTypeName(field.Type)
					// An embedded unexported non-struct type is ignored.
					if typeName == "" || (!isStruct && !ast.IsExported(typeName)) {
						continue
					}
					names = append(names, typeName)
				} else {
					for _, ident := range field.Names {
						if ast.IsExported(ident.Name) {
							names = append(names, ident.Name)
						}
					}
				}

				for n, goName := range names {
					promoted := promotedField{
						name:     jsonFieldName(goName, field),
						field:    field,
						imports:  entry.decl.imports,
						pkgPath:  entry.decl.pkgPath,
						depth:    entry.depth,
						tagged:   tagName != "",
						top:      top,
						optional: entry.optional,
						index:    index,
						generic:  entry.generic.name,
					}
					// The names of one declaration, A, B int, are fields of their own.
					if entry.depth == 0 && n > 0 {
						promoted.top = own
						own++
					}
					walk.fields = append(walk.fields, promoted)
					if count[entry.decl.ref] > 1 {
						// The copy stands for another path to the struct, which may start at
						// another own field: it collides.
						duplicate := promoted
						duplicate.top = -1
						walk.fields = append(walk.fields, duplicate)
					}
				}
			}
		}
	}

	return walk
}

// embeddedStructDecl is the struct declaration whose fields an embedded type promotes. ok is
// false for a type the generator does not read that way: one described in Config.TypeSchemas
// without a marshaler, a well-known, standard-library or unresolved one. An instantiation of a
// generic type other than sql.Null[T], an alias of one and a type declared from one are read
// as the generic struct (genericStruct), for its fields' names. A described
// type with a marshaler (describedMarshaler) is read from source, as its description is of the
// method's output, not of the fields a struct that does not get the method writes. A
// standard-library or well-known struct with a marshal method has unexported fields only, so it
// is read as the struct{} knownUnderlying lists, as is a type declared from one
// (structFromSource). database/sql's Null types, sql.Null[T] included, and the types declared
// from them are read as the structs they are. An alias resolved to a named type (aliasTarget) is
// read as that type, which is the declaration's ref; an alias of an instantiation,
// `type N = sql.Null[int]`, keeps its own name.
func (b *schemaBuilder) embeddedStructDecl(expr ast.Expr, imports map[string]string, pkgPath string) (sourceStruct, bool) {
	generic := genericType(expr)
	ref, ok := typeRefFromExpr(generic, imports, pkgPath)
	if !ok {
		return sourceStruct{}, false
	}
	ref = b.aliasTarget(ref)
	if generic != expr && !isSQLNull(ref) {
		return b.genericStruct(expr, imports, pkgPath)
	}
	_, described := b.typeSchemas[typeKey(ref)]
	if described && !b.describedMarshaler(ref) {
		return sourceStruct{}, false
	}
	if generic != expr {
		return sqlNullDecl(expr, imports, pkgPath)
	}
	null, ok := sqlNullStruct(ref)
	if ok {
		return null, true
	}
	listed, ok := knownUnderlyingExpr(ref)
	if ok {
		structure, isStruct := listed.(*ast.StructType)
		return sourceStruct{ref: ref, st: structure}, isStruct
	}
	_, wellKnown := b.wellKnownComponentSchema(ref)
	if wellKnown {
		return sourceStruct{}, false
	}

	return b.structFromSource(ref)
}

// marshalsItself reports an embedded struct type whose component is not its fields: it declares
// MarshalJSON or MarshalText (methodsOf, a described or standard-library type included), or
// promotedMarshaler finds one promoted into it or cannot tell whether one is. Its component is
// then a string, a description of the method's output, or fails generation, while a struct that
// embeds it without getting the method still gets its fields.
func (b *schemaBuilder) marshalsItself(decl sourceStruct) bool {
	methods, _ := b.methodsOf(decl.ref)
	if methods["MarshalJSON"] || methods["MarshalText"] {
		return true
	}

	return b.promotedMarshaler(decl.st, decl.imports, decl.pkgPath, false) != ""
}

// sqlNullGoTypes are the value fields' Go types of database/sql's Null types.
var sqlNullGoTypes = map[string]string{
	"NullBool":    "bool",
	"NullByte":    "byte",
	"NullFloat64": "float64",
	"NullInt16":   "int16",
	"NullInt32":   "int32",
	"NullInt64":   "int64",
	"NullString":  "string",
	"NullTime":    "time.Time",
}

// sqlNullStruct is the declaration of one of database/sql's Null types, which is not read from
// source.
func sqlNullStruct(ref typeRef) (sourceStruct, bool) {
	goType, ok := sqlNullGoTypes[ref.Name]
	if !ok || ref.ImportPath != "database/sql" {
		return sourceStruct{}, false
	}
	expr, err := parser.ParseExpr("struct {\n" + sqlNullValues[ref.Name].Field + " " + goType + "\nValid bool\n}")
	if err != nil {
		return sourceStruct{}, false
	}
	structure, ok := expr.(*ast.StructType)
	if !ok {
		return sourceStruct{}, false
	}

	return sourceStruct{ref: ref, st: structure, imports: map[string]string{"time": "time"}, pkgPath: ref.ImportPath}, true
}

// sqlNullDecl is the declaration of the database/sql Null type expr names: one of the named
// ones (sqlNullStruct), or sql.Null[T], the struct {V T; Valid bool} with T read where expr is
// written. Its ref names the instantiation by T's package and name, so that two embeddings of
// one instantiation at one depth tie as in encoding/json.
func sqlNullDecl(expr ast.Expr, imports map[string]string, pkgPath string) (sourceStruct, bool) {
	index, isIndex := expr.(*ast.IndexExpr)
	if !isIndex {
		ref, ok := namedTypeRef(expr, imports, pkgPath)
		if !ok {
			return sourceStruct{}, false
		}
		return sqlNullStruct(ref)
	}
	generic, ok := typeRefFromExpr(index.X, imports, pkgPath)
	if !ok || !isSQLNull(generic) {
		return sourceStruct{}, false
	}

	argument := types.ExprString(index.Index)
	named, ok := namedTypeRef(index.Index, imports, pkgPath)
	if ok {
		argument = typeKey(named)
	}
	structure := &ast.StructType{Fields: &ast.FieldList{List: []*ast.Field{
		{Names: []*ast.Ident{ast.NewIdent("V")}, Type: index.Index},
		{Names: []*ast.Ident{ast.NewIdent("Valid")}, Type: ast.NewIdent("bool")},
	}}}
	ref := typeRef{ImportPath: generic.ImportPath, Name: generic.Name + "[" + argument + "]"}

	return sourceStruct{ref: ref, st: structure, imports: imports, pkgPath: pkgPath}, true
}

// genericEmbedReason explains why an embedded instantiation of a generic type cannot be the
// allOf member referencing its description in a merged struct, "" when it can. The walk has its
// fields' names, which do not depend on the type arguments, but not their types, so it can be
// such a member only when Go writes exactly the fields the description has: not when it is
// embedded through a pointer, a nil one leaving them out; nor when one of its field names is
// shared with another field the struct writes or drops, among the fields the walk reads (those
// of an embedded type it does not expand are not known to it); nor when it has MarshalJSON or
// MarshalText, which a tie leaves unpromoted, as its description is then of the method's output;
// nor when its declaration embeds a type, whose promoted fields the walk does not resolve inside
// a generic type.
func (b *schemaBuilder) genericEmbedReason(generic genericEmbed, fields []promotedField) string {
	var why []string
	if generic.optional {
		why = append(why, "it is embedded through a pointer, and a nil one leaves its fields out")
	}
	shared := sharedGenericName(generic.name, fields)
	if shared != "" {
		why = append(why, "its field "+strconv.Quote(shared)+" shares its name with another field of the struct, so encoding/json writes that other field or neither")
	}
	methods, _ := b.methodsOf(generic.decl.ref)
	for _, method := range []string{"MarshalJSON", "MarshalText"} {
		if methods[method] {
			why = append(why, "it has "+method+", which is not promoted here, so encoding/json writes its fields while its description is of "+method+"'s output")
			break
		}
	}
	for _, field := range generic.decl.st.Fields.List {
		_, skipped := jsonTagName(field)
		if field != nil && len(field.Names) == 0 && !skipped {
			why = append(why, "its declaration embeds "+types.ExprString(field.Type)+", whose promoted fields the generator does not resolve in a generic type")
			break
		}
	}
	if len(why) == 0 {
		return ""
	}

	return "embeds " + generic.name + ", an instantiation of a generic type whose fields' types the generator does not instantiate, so it can only be the allOf member referencing its description, and that is not what encoding/json writes: " + strings.Join(why, ", and ")
}

// sharedGenericName is a field name of the embedded generic named that another field of the
// struct also has: one of its own, one another embedded type promotes, or the same generic's
// reached by another path. "" when there is none.
func sharedGenericName(name string, fields []promotedField) string {
	for _, field := range fields {
		if field.generic != name {
			continue
		}
		for _, other := range fields {
			if other.name == field.name && (other.generic != name || other.top != field.top) {
				return field.name
			}
		}
	}

	return ""
}

// collides reports a name that two of the struct's own fields have, or one of them and a field
// an embedded struct promotes, or two embedded structs. A collision inside one embedded struct is
// that struct's own and its schema already resolves it.
func collides(fields []promotedField) bool {
	tops := map[string]int{}
	for _, promoted := range fields {
		top, seen := tops[promoted.name]
		if seen && top != promoted.top {
			return true
		}
		tops[promoted.name] = promoted.top
	}

	return false
}

// dominantFields keeps the field encoding/json writes for each name, in the order it writes
// them: the shallowest one, or among several at that depth the only one with a json tag. A name
// with no such field is dropped.
func dominantFields(fields []promotedField) []promotedField {
	byName := map[string][]int{}
	for i, promoted := range fields {
		byName[promoted.name] = append(byName[promoted.name], i)
	}

	var kept []promotedField
	for i, promoted := range fields {
		dominant, ok := dominantField(fields, byName[promoted.name])
		if ok && dominant == i {
			kept = append(kept, promoted)
		}
	}
	slices.SortStableFunc(kept, func(a promotedField, b promotedField) int {
		return slices.Compare(a.index, b.index)
	})

	return kept
}

// dominantField is the position, among the fields at positions, of the one encoding/json writes
// for their shared name.
func dominantField(fields []promotedField, positions []int) (int, bool) {
	shallowest := fields[positions[0]].depth
	for _, position := range positions {
		shallowest = min(shallowest, fields[position].depth)
	}

	var atDepth []int
	var tagged []int
	for _, position := range positions {
		if fields[position].depth != shallowest {
			continue
		}
		atDepth = append(atDepth, position)
		if fields[position].tagged {
			tagged = append(tagged, position)
		}
	}
	if len(atDepth) == 1 {
		return atDepth[0], true
	}
	if len(tagged) == 1 {
		return tagged[0], true
	}

	return 0, false
}
