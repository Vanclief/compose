package generator

import (
	"sort"
	"strconv"
	"strings"
)

// resource is a package routed handlers take their request types from, e.g.
// application/modules/finance/bankaccounts under the Finance tag.
type resource struct {
	pkgPath string
	tag     string
	// returns counts, per component, the handlers returning it directly.
	returns  map[string]int
	handlers int
	entity   string
	name     string
}

// tagResources nests a child tag per resource under its operation tag, in the document's
// tags as {name, x-parent} (parent from OpenAPI 3.2 on), and files the resource's schemas
// under it as x-tags: the package's own schemas, the models its handlers return (a model
// several resources return goes to the one named like it, else the one returning it most,
// else stays under its operation tag) and the enums and scalars nothing else uses. Operation
// tags stay as they are.
func (g *generator) tagResources(document map[string]any, routes []Route, facts handlerFacts, handlerSchemas map[string]handlerSchema) {
	schemas := g.builder.schemas
	resources := g.collectResources(routes, facts, handlerSchemas)
	if len(resources) == 0 {
		return
	}

	existing := map[string]bool{}
	tags, _ := document["tags"].([]any)
	for _, tag := range tags {
		typed, _ := tag.(map[string]any)
		name, _ := typed["name"].(string)
		existing[name] = true
	}

	// Name every resource, dropping those named like their operation tag and qualifying names
	// several resources, or an operation tag, would share.
	var named []*resource
	byName := map[string][]*resource{}
	for _, res := range resources {
		res.entity = mostReturned(res, g.builder.origins)
		entity := g.builder.origins[res.entity].Name
		name := resourceName(entity, res.pkgPath)
		if g.cfg.ResourceTag != nil {
			name = g.cfg.ResourceTag(res.pkgPath, entity, name)
		}
		if name == "" || sameName(name, res.tag) {
			continue
		}
		res.name = name
		named = append(named, res)
		byName[name] = append(byName[name], res)
	}
	for name, sharing := range byName {
		if len(sharing) == 1 && !existing[name] {
			continue
		}
		for _, res := range sharing {
			res.name = res.tag + " " + lowerFirstWord(name)
		}
	}

	owner := map[string]*resource{}
	for _, res := range named {
		for name, schema := range schemas {
			if g.builder.origins[name].ImportPath == res.pkgPath && schema != nil {
				owner[name] = res
			}
		}
	}
	for name, res := range modelOwners(named, g.builder.origins) {
		if owner[name] == nil {
			owner[name] = res
		}
	}

	// Enums and scalars only one resource's schemas refer to join it.
	users := map[string]map[*resource]bool{}
	for name, res := range owner {
		refs := map[string]bool{}
		collectRefs(schemas[name], refs)
		for ref := range refs {
			if owner[ref] != nil || !isLeafSchema(schemas[ref]) {
				continue
			}
			if users[ref] == nil {
				users[ref] = map[*resource]bool{}
			}
			users[ref][res] = true
		}
	}
	for ref, by := range users {
		if len(by) != 1 || !isLeafSchema(schemas[ref]) {
			continue
		}
		for res := range by {
			owner[ref] = res
		}
	}

	for name, res := range owner {
		setSchemaTags(schemas[name], []string{res.name})
	}

	parentKey := "x-parent"
	if openAPIAtLeast(document["openapi"], 3, 2) {
		parentKey = "parent"
	}
	sort.Slice(named, func(i int, j int) bool {
		if named[i].tag != named[j].tag {
			return named[i].tag < named[j].tag
		}
		return named[i].name < named[j].name
	})
	for _, res := range named {
		tag := map[string]any{"name": res.name, parentKey: res.tag}
		packageDoc := ""
		pkg, ok := g.builder.packages[res.pkgPath]
		if ok {
			packageDoc = pkg.Doc
		}
		description := g.tagDescription(res.name, packageDoc)
		if description != "" {
			tag["description"] = description
		}
		tags = append(tags, tag)
	}
	document["tags"] = tags
}

// collectResources groups the documented routes by the module package of their request type.
func (g *generator) collectResources(routes []Route, facts handlerFacts, handlerSchemas map[string]handlerSchema) []*resource {
	byPackage := map[string]*resource{}
	var out []*resource
	for _, r := range routes {
		if r.Deprecated || g.skip(r.Method, toOpenAPIPath(r.Path)) {
			continue
		}
		ref, ok := facts.requestTypes[r.HandlerKey()]
		if !ok || !strings.HasPrefix(ref.ImportPath, g.builder.modulePath+"/") {
			continue
		}

		tag := g.tagForNamespace(r.Namespace)
		key := tag + " " + ref.ImportPath
		res, ok := byPackage[key]
		if !ok {
			res = &resource{pkgPath: ref.ImportPath, tag: tag, returns: map[string]int{}}
			byPackage[key] = res
			out = append(out, res)
		}
		res.handlers++

		returned := componentNameFromSchema(handlerSchemas[r.HandlerKey()].Response)
		if returned != "" && g.builder.origins[returned].ImportPath != ref.ImportPath {
			res.returns[returned]++
		}
	}

	// A package serving several tags is no one resource.
	tagsOf := map[string]int{}
	for _, res := range out {
		tagsOf[res.pkgPath]++
	}
	var kept []*resource
	for _, res := range out {
		if tagsOf[res.pkgPath] == 1 {
			kept = append(kept, res)
		}
	}

	return kept
}

// mostReturned is the model a resource's handlers return most, ties broken by the one named
// like the package, then by name.
func mostReturned(res *resource, origins map[string]typeRef) string {
	best := ""
	for _, name := range sortedKeys(res.returns) {
		if best == "" || res.returns[name] > res.returns[best] {
			best = name
			continue
		}
		if res.returns[name] == res.returns[best] && namedLike(origins[name].Name, res.pkgPath) && !namedLike(origins[best].Name, res.pkgPath) {
			best = name
		}
	}

	return best
}

// modelOwners files every model the resources return under one of them: the one whose package
// is named like the model, else the one returning it from most handlers. Ties are left out.
func modelOwners(resources []*resource, origins map[string]typeRef) map[string]*resource {
	returners := map[string][]*resource{}
	for _, res := range resources {
		for name := range res.returns {
			returners[name] = append(returners[name], res)
		}
	}

	owners := map[string]*resource{}
	for name, candidates := range returners {
		if len(candidates) == 1 {
			owners[name] = candidates[0]
			continue
		}

		var alike []*resource
		for _, res := range candidates {
			if namedLike(origins[name].Name, res.pkgPath) {
				alike = append(alike, res)
			}
		}
		if len(alike) == 1 {
			owners[name] = alike[0]
			continue
		}

		var best *resource
		tie := false
		for _, res := range candidates {
			switch {
			case best == nil || res.returns[name] > best.returns[name]:
				best = res
				tie = false
			case res.returns[name] == best.returns[name]:
				tie = true
			}
		}
		if !tie {
			owners[name] = best
		}
	}

	return owners
}

// namedLike reports a Go type named like the package: BankAccount and bankaccounts.
func namedLike(typeName string, pkgPath string) bool {
	return sameName(typeName, pkgPath[strings.LastIndex(pkgPath, "/")+1:])
}

// resourceName humanises and pluralises the entity ("BankAccount" is "Bank accounts", "APIKey"
// is "API keys"), else the package name ("overview" is "Overview").
func resourceName(entity string, pkgPath string) string {
	if entity == "" {
		name := strings.NewReplacer("_", " ", "-", " ").Replace(pkgPath[strings.LastIndex(pkgPath, "/")+1:])
		if name == "" {
			return ""
		}
		return strings.ToUpper(name[:1]) + name[1:]
	}

	words := splitCamelWords(entity)
	for i := 1; i < len(words); i++ {
		if strings.ToUpper(words[i]) != words[i] {
			words[i] = strings.ToLower(words[i])
		}
	}
	words[len(words)-1] = plural(words[len(words)-1])

	return strings.Join(words, " ")
}

func plural(word string) string {
	lower := strings.ToLower(word)
	switch {
	case strings.HasSuffix(lower, "y") && len(lower) > 1 && !strings.ContainsRune("aeiou", rune(lower[len(lower)-2])):
		return word[:len(word)-1] + "ies"
	case strings.HasSuffix(lower, "s"), strings.HasSuffix(lower, "x"), strings.HasSuffix(lower, "z"),
		strings.HasSuffix(lower, "ch"), strings.HasSuffix(lower, "sh"):
		return word + "es"
	}

	return word + "s"
}

func lowerFirstWord(name string) string {
	first, rest, _ := strings.Cut(name, " ")
	if strings.ToUpper(first) != first {
		first = strings.ToLower(first)
	}
	if rest == "" {
		return first
	}

	return first + " " + rest
}

// isLeafSchema reports an enum or scalar component: no properties, no composition.
func isLeafSchema(schema map[string]any) bool {
	if schema == nil {
		return false
	}
	for _, key := range []string{"properties", "allOf", "oneOf", "anyOf", "items", "additionalProperties"} {
		_, ok := schema[key]
		if ok {
			return false
		}
	}

	return schema["type"] != "object" && schema["type"] != "array"
}

// openAPIAtLeast reports a document version of at least major.minor.
func openAPIAtLeast(version any, major int, minor int) bool {
	text, _ := version.(string)
	parts := strings.Split(text, ".")
	if len(parts) < 2 {
		return false
	}
	gotMajor, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	gotMinor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}

	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}
