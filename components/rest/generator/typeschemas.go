package generator

// extendsGeneratedKey marks a Config.TypeSchemas entry made by ExtendGenerated. Its value lists
// the properties to leave out. It never reaches the document.
const extendsGeneratedKey = "x-generator-extends-generated"

// ExtendGenerated returns a Config.TypeSchemas entry that is the schema the generator derives
// from the Go type, changed by extra: extra's properties are added to the type's own ones,
// replacing those of the same name, its required names are added to the type's required list,
// any other keyword of extra replaces the derived one, and the omit properties are left out.
//
// It describes the usual custom MarshalJSON, which marshals an alias of the type inside a
// struct that adds or shadows a few fields:
//
//	cfg.TypeSchemas = map[string]map[string]any{
//		"example.com/app/models.User": generator.ExtendGenerated(map[string]any{
//			"properties": map[string]any{"role": roleObject},
//			"required":   []any{"role"},
//		}),
//	}
func ExtendGenerated(extra map[string]any, omit ...string) map[string]any {
	schema := cloneSchema(extra)
	schema[extendsGeneratedKey] = append([]string{}, omit...)

	return schema
}

// isExtension reports a TypeSchemas entry made by ExtendGenerated.
func isExtension(schema map[string]any) bool {
	_, ok := schema[extendsGeneratedKey]
	return ok
}

// extendedSchema applies an ExtendGenerated entry to the schema derived from the Go type. Any
// other override (nil included) leaves generated as is.
func extendedSchema(generated map[string]any, override map[string]any, b *schemaBuilder) map[string]any {
	if !isExtension(override) {
		return generated
	}

	extra := cloneSchema(override)
	omit, _ := extra[extendsGeneratedKey].([]string)
	delete(extra, extendsGeneratedKey)

	own := ownMember(generated)
	properties, ok := own["properties"].(map[string]any)
	if !ok {
		properties = map[string]any{}
		own["properties"] = properties
	}

	for _, name := range omit {
		delete(properties, name)
	}
	extraProperties, _ := extra["properties"].(map[string]any)
	for name, property := range extraProperties {
		properties[name] = property
	}
	delete(extra, "properties")

	var required []string
	for _, name := range requiredNames(own["required"]) {
		if !containsString(omit, name) {
			required = append(required, name)
		}
	}
	for _, name := range requiredNames(extra["required"]) {
		if !containsString(required, name) {
			required = append(required, name)
		}
	}
	delete(extra, "required")
	if len(required) > 0 {
		// Like the derived names, these describe what the server serializes; the request side
		// drops them (splitRequestSide).
		own["required"] = required
		b.markDerived(required)
	} else {
		delete(own, "required")
	}

	for key, value := range extra {
		generated[key] = value
	}

	return generated
}

// ownMember is the object holding a struct schema's own properties: the schema itself, or
// the inline member of the allOf schemaForStruct splits embedded types into.
func ownMember(schema map[string]any) map[string]any {
	allOf, ok := schema["allOf"].([]any)
	if !ok {
		return schema
	}

	for i := len(allOf) - 1; i >= 0; i-- {
		member, ok := allOf[i].(map[string]any)
		if !ok {
			continue
		}
		_, isRef := member["$ref"]
		_, hasProperties := member["properties"]
		if !isRef && hasProperties {
			return member
		}
	}

	member := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	schema["allOf"] = append(allOf, member)

	return member
}
