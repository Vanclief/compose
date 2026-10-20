package generator

import (
	"sort"
	"strings"
)

// tagSchemas files every component schema under the operation tags deriveSchemaTags finds for
// its Go package, as x-tags, when exactly one is found or Config.SchemaTags picks them. A
// schema used across tags, or from a package no namespace claims, gets none: readers fall back
// to where the schema is used.
func (g *generator) tagSchemas(routes []Route, facts handlerFacts) {
	byPackage := map[string]map[string]bool{}
	namespaces := map[string]string{}
	for _, r := range routes {
		if r.Deprecated || g.skip(r.Method, toOpenAPIPath(r.Path)) {
			continue
		}

		tag := g.tagForNamespace(r.Namespace)
		if r.Namespace != "" {
			namespaces[r.Namespace] = tag
		}

		ref, ok := facts.requestTypes[r.HandlerKey()]
		if !ok {
			continue
		}
		if byPackage[ref.ImportPath] == nil {
			byPackage[ref.ImportPath] = map[string]bool{}
		}
		byPackage[ref.ImportPath][tag] = true
	}

	for name, schema := range g.builder.schemas {
		pkgPath := g.builder.origins[name].ImportPath
		derived := deriveSchemaTags(pkgPath, g.builder.modulePath, byPackage, namespaces)

		tags := derived
		if g.cfg.SchemaTags != nil {
			tags = g.cfg.SchemaTags(name, pkgPath, derived)
		} else if len(derived) != 1 {
			tags = nil
		}
		if len(tags) == 0 {
			delete(schema, "x-tags")
			continue
		}
		setSchemaTags(schema, tags)
	}
}

// setSchemaTags writes a component's x-tags. A component that is a bare $ref (an alias a
// Schemas hook wrote) becomes an allOf wrapper first: OpenAPI 3.0 ignores the siblings of a $ref.
func setSchemaTags(schema map[string]any, tags []string) {
	ref, isRef := schema["$ref"]
	if isRef {
		delete(schema, "$ref")
		schema["allOf"] = []any{map[string]any{"$ref": ref}}
	}
	schema["x-tags"] = tags
}

// deriveSchemaTags returns the tags of a package's schemas, sorted:
//   - the tags of the routes whose handlers take their request type from the package (a
//     module's requests and responses), or else
//   - the tag of the namespace named like the package or its closest parent inside the module
//     (application/models/accesscontrol and its subpackages under accessControl, models/widget
//     under widgets).
//
// Packages outside the module and packages nothing claims get none.
func deriveSchemaTags(pkgPath string, modulePath string, byPackage map[string]map[string]bool, namespaces map[string]string) []string {
	used := byPackage[pkgPath]
	if len(used) > 0 {
		return sortedKeys(used)
	}

	if modulePath == "" || !strings.HasPrefix(pkgPath, modulePath+"/") {
		return nil
	}

	elements := strings.Split(strings.TrimPrefix(pkgPath, modulePath+"/"), "/")
	for i := len(elements) - 1; i >= 0; i-- {
		var tags []string
		for namespace, tag := range namespaces {
			if sameName(elements[i], namespace) {
				tags = append(tags, tag)
			}
		}
		if len(tags) > 0 {
			sort.Strings(tags)
			return uniqueStrings(tags)
		}
	}

	return nil
}

// sameName matches a package name against a namespace ignoring case, separators and a plural
// ending: accesscontrol is accessControl, widget is widgets, amenity is amenities.
func sameName(pkgName string, namespace string) bool {
	a := normalizedName(pkgName)
	b := normalizedName(namespace)
	if a == "" || b == "" {
		return false
	}

	return singular(a) == singular(b)
}

func normalizedName(name string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(name))
}

func singular(name string) string {
	switch {
	case strings.HasSuffix(name, "ies"):
		return strings.TrimSuffix(name, "ies") + "y"
	case strings.HasSuffix(name, "ss"):
		return name
	case strings.HasSuffix(name, "s"):
		return strings.TrimSuffix(name, "s")
	}

	return name
}

func uniqueStrings(sorted []string) []string {
	var out []string
	for i, value := range sorted {
		if i == 0 || value != sorted[i-1] {
			out = append(out, value)
		}
	}

	return out
}
