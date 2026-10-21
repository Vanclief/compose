package generator

import (
	"slices"
	"strings"

	"github.com/vanclief/ez"
)

// RequestVariantSuffix names the request-side copy of a component that requests and responses
// share but that cannot require the same fields on both: a model the server always serializes
// in full, also accepted in a request body, is application_models_finance_IncomeToInvoice in
// responses and application_models_finance_IncomeToInvoiceInput in requests.
const RequestVariantSuffix = "Input"

// markDerived records a required list the builder derived from the value-typed,
// non-omitempty rule, so the request side can tell it from one a Schemas hook wrote. It keeps a
// copy of the list under the address of its first name: a list a hook extended (even in place,
// by append into spare capacity), shortened, reordered or replaced no longer matches it.
func (b *schemaBuilder) markDerived(required []string) {
	if len(required) == 0 {
		return
	}
	if b.derivedRequired == nil {
		b.derivedRequired = map[*string][]string{}
	}
	b.derivedRequired[&required[0]] = slices.Clone(required)
}

// isDerived reports a required list markDerived recorded, with the same names in the same
// order. A hook that changed or replaced the list owns it.
func (b *schemaBuilder) isDerived(required any) bool {
	list, ok := required.([]string)
	if !ok || len(list) == 0 {
		return false
	}

	recorded, ok := b.derivedRequired[&list[0]]
	if !ok {
		return false
	}

	return slices.Equal(recorded, list)
}

// isModel reports a component built from a RequiredPackagePrefix package.
func (b *schemaBuilder) isModel(name string) bool {
	if b.requiredPackagePrefix == "" {
		return false
	}

	return strings.Contains(b.origins[name].ImportPath, b.requiredPackagePrefix)
}

// requestSideRequired is the required list a derived one keeps on the request side: only the
// readOnly properties, which OpenAPI 3.0 scopes to responses. ok is false when the list is not
// derived (a hook's) and so stays as is.
func (b *schemaBuilder) requestSideRequired(member map[string]any) ([]string, bool) {
	if !b.isDerived(member["required"]) {
		return nil, false
	}

	properties, _ := member["properties"].(map[string]any)
	var kept []string
	for _, name := range requiredNames(member["required"]) {
		property, ok := properties[name].(map[string]any)
		if ok && property["readOnly"] == true {
			kept = append(kept, name)
		}
	}

	return kept, true
}

// stripsRequired reports a schema whose request side requires less than its response side.
func (b *schemaBuilder) stripsRequired(schema map[string]any) bool {
	for _, member := range schemaMembers(schema) {
		kept, ok := b.requestSideRequired(member)
		if ok && len(kept) != len(requiredNames(member["required"])) {
			return true
		}
	}

	return false
}

// stripRequired drops the derived required names a request cannot be held to.
func (b *schemaBuilder) stripRequired(schema map[string]any) {
	for _, member := range schemaMembers(schema) {
		kept, ok := b.requestSideRequired(member)
		if !ok {
			continue
		}
		if len(kept) == 0 {
			delete(member, "required")
			continue
		}
		member["required"] = kept
		b.markDerived(kept)
	}
}

// splitRequestSide applies the request-side required rules. Go reads a missing request key as
// its zero value, so no request can be required to send a field just because the struct has
// no omitempty; the value-typed, non-omitempty rule describes what the server serializes.
//
//   - A component only requests reach loses its derived required names (readOnly ones stay).
//   - A component requests and responses share loses them too, unless it is a model (a
//     RequiredPackagePrefix package): the server always serializes those in full, so the
//     component keeps its list for the responses and the requests get a copy without it, named
//     with RequestVariantSuffix. A shared component that reaches such a copy gets one as well.
//   - A component only responses reach keeps its list.
//
// Required lists a Schemas hook wrote are the hook's and stay as they are. The handlers'
// request schemas are rewritten to reference the copies.
func (b *schemaBuilder) splitRequestSide(handlerSchemas map[string]handlerSchema) error {
	schemas := b.schemas

	var requestRoots []any
	var responseRoots []any
	for _, key := range sortedKeys(handlerSchemas) {
		requestRoots = append(requestRoots, handlerSchemas[key].Request)
		responseRoots = append(responseRoots, handlerSchemas[key].Response)
	}
	requestSide := reachableComponents(requestRoots, schemas)
	responseSide := reachableComponents(responseRoots, schemas)

	variants := map[string]bool{}
	for name := range requestSide {
		if responseSide[name] && b.isModel(name) && b.stripsRequired(schemas[name]) {
			variants[name] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for name := range requestSide {
			if variants[name] || !responseSide[name] {
				continue
			}
			refs := map[string]bool{}
			collectRefs(schemas[name], refs)
			for ref := range refs {
				if variants[ref] {
					variants[name] = true
					changed = true
					break
				}
			}
		}
	}

	for _, name := range sortedKeys(requestSide) {
		schema, ok := schemas[name]
		if !ok {
			continue
		}

		if variants[name] {
			variantName := name + RequestVariantSuffix
			_, taken := schemas[variantName]
			if taken {
				return ez.New(ez.EINVALID, "request variant "+variantName+" of "+typeKey(b.origins[name])+" collides with an existing component", nil)
			}
			variant := cloneSchema(schema)
			b.stripRequired(variant)
			rewriteVariantRefs(variant, variants)
			schemas[variantName] = variant
			b.origins[variantName] = b.origins[name]
			b.variantOf[variantName] = name
			if !b.isModel(name) {
				b.stripRequired(schema)
			}
			continue
		}

		if responseSide[name] && b.isModel(name) {
			continue
		}
		b.stripRequired(schema)
		rewriteVariantRefs(schema, variants)
	}

	for _, key := range sortedKeys(handlerSchemas) {
		rewriteVariantRefs(handlerSchemas[key].Request, variants)
	}

	return nil
}

// reachableComponents returns the components the roots reference, directly or through other
// components.
func reachableComponents(roots []any, schemas map[string]map[string]any) map[string]bool {
	pending := map[string]bool{}
	for _, root := range roots {
		collectRefs(root, pending)
	}

	reached := map[string]bool{}
	for len(pending) > 0 {
		names := sortedKeys(pending)
		name := names[0]
		delete(pending, name)
		if reached[name] {
			continue
		}
		_, ok := schemas[name]
		if !ok {
			continue
		}
		reached[name] = true
		collectRefs(schemas[name], pending)
	}

	return reached
}

// rewriteVariantRefs points every $ref under node at the request variant of its component.
func rewriteVariantRefs(node any, variants map[string]bool) {
	switch typed := node.(type) {
	case map[string]any:
		name := componentNameFromSchema(typed)
		if variants[name] {
			typed["$ref"] = schemaRefPrefix + name + RequestVariantSuffix
		}
		for _, value := range typed {
			rewriteVariantRefs(value, variants)
		}
	case []any:
		for _, value := range typed {
			rewriteVariantRefs(value, variants)
		}
	}
}

func requiredNames(required any) []string {
	switch typed := required.(type) {
	case []string:
		return typed
	case []any:
		var names []string
		for _, name := range typed {
			text, ok := name.(string)
			if ok {
				names = append(names, text)
			}
		}
		return names
	default:
		return nil
	}
}
