package mcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vanclief/compose/components/rest/reference"
)

// maxSuggestions bounds the "did you mean" list of unknown names.
const maxSuggestions = 5

// maxFoldedResults is the most functions a search shows with their route and
// arguments. Larger results keep one dense line per function.
const maxFoldedResults = 5

// matchedModule is a module header and the functions of it a search shows.
type matchedModule struct {
	header    string
	functions []*Function
}

// renderSearch lists modules, the functions of one module, or the functions
// matching a free-text query, optionally within one module. When the
// functions listed are maxFoldedResults or fewer, each comes as its folded
// block.
func (c *Catalog) renderSearch(module, query string) string {
	module = strings.TrimSpace(module)
	query = strings.TrimSpace(query)

	modules := c.Modules
	if module != "" {
		m, ok := c.Module(snake(module))
		if !ok {
			return fmt.Sprintf("unknown module %q; modules: %s", module, c.moduleNames())
		}
		modules = []*Module{m}
	}

	if query == "" && module == "" {
		var lines []string
		for _, m := range c.Modules {
			line := m.Name + "  " + plural(len(m.Functions), "function")
			description := firstLine(m.Description)
			if description != "" {
				line += "  " + description
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}

	words := searchWords(query)
	// A query of only underscores and punctuation leaves no words, which
	// every function would match.
	if query != "" && len(words) == 0 {
		return fmt.Sprintf("no functions match %q\nmodules: %s", query, c.moduleNames())
	}
	var matched []matchedModule
	total := 0
	for _, m := range modules {
		var functions []*Function
		for _, f := range m.Functions {
			if functionMatches(f, words) {
				functions = append(functions, f)
			}
		}
		if len(functions) == 0 && query != "" {
			continue
		}
		header := "module " + m.Name
		description := firstLine(m.Description)
		if query == "" && description != "" {
			header += ": " + description
		}
		matched = append(matched, matchedModule{header: header, functions: functions})
		total += len(functions)
	}
	if len(matched) == 0 {
		return fmt.Sprintf("no functions match %q\nmodules: %s", query, c.moduleNames())
	}

	folded := total <= maxFoldedResults
	var lines []string
	for _, m := range matched {
		if folded && len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, m.header)
		for i, f := range m.functions {
			if !folded {
				lines = append(lines, "  "+c.searchLine(f))
				continue
			}
			if i > 0 {
				lines = append(lines, "")
			}
			for _, line := range c.foldedBlock(f) {
				lines = append(lines, "  "+line)
			}
		}
	}
	return strings.Join(lines, "\n")
}

// foldedBlock is the entry of a function in a search with few results: its
// search line, then its route, description and arguments as
// describe_function prints them, without the return shape or related terms.
// The route is left out when the search line already says it: a composite
// carries its tag, and a summary can be the route the catalog fell back to.
func (c *Catalog) foldedBlock(f *Function) []string {
	lines := []string{c.searchLine(f)}
	summaryIsRoute := strings.TrimSpace(f.Summary) == f.Method+" "+f.Path
	if !f.Composite && !summaryIsRoute {
		lines = append(lines, "  "+route(f))
	}
	return append(lines, c.detailLines(f)...)
}

// searchLine is the one-line entry of a function in search results.
func (c *Catalog) searchLine(f *Function) string {
	line := f.Name
	if f.Summary != "" {
		line += "  " + f.Summary
	}
	returns := c.returnLabel(f)
	if returns != "" {
		line += "  -> " + returns
	}
	if f.Composite {
		line += " [composite]"
	}
	if f.Deprecated {
		line += " [deprecated]"
	}
	return line
}

// returnLabel names what a function returns: its term, else the type of an
// inline array or scalar response. Inline objects have no useful short name.
func (c *Catalog) returnLabel(f *Function) string {
	if f.Returns != "" {
		return f.Returns
	}
	if f.ReturnSchema == nil {
		return ""
	}
	label := c.typeName(f.ReturnSchema)
	if label == "object" || label == "any" {
		return ""
	}
	return label
}

// searchWords splits a query into lowercase words. Underscores count as
// spaces, so "register_package" and "register package" are the same query.
// A word without a letter or a digit, such as "." or "-", is dropped: a dot
// alone would match the whole catalog, as every function name holds one.
func searchWords(query string) []string {
	var words []string
	for _, word := range strings.Fields(strings.ReplaceAll(strings.ToLower(query), "_", " ")) {
		if strings.IndexFunc(word, isLetterOrDigit) >= 0 {
			words = append(words, word)
		}
	}
	return words
}

func isLetterOrDigit(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// minSingularLen is the fewest letters a word may keep after dropping a
// plural s and still count as a match, so "ids" does not match every "id".
const minSingularLen = 3

// functionMatches reports whether every word occurs in the name, summary,
// description, return term or an argument name of f. Each word may match a
// different field. A word ending in s also matches without the s, so
// "payments" finds what "payment" finds.
//
// grug: only a trailing s is dropped, so "companies" does not find "company".
// Add the other plural forms when searches that miss on them show up.
func functionMatches(f *Function, words []string) bool {
	fields := []string{f.Name, f.Summary, f.Description, f.Returns}
	for _, a := range f.Args {
		fields = append(fields, a.Name)
	}
	text := strings.ReplaceAll(strings.ToLower(strings.Join(fields, "\n")), "_", " ")
	for _, word := range words {
		if strings.Contains(text, word) {
			continue
		}
		singular, ok := strings.CutSuffix(word, "s")
		if !ok || utf8.RuneCountInString(singular) < minSingularLen || !strings.Contains(text, singular) {
			return false
		}
	}
	return true
}

// summaryLine is the summary describe_function and a promoted tool show:
// the trimmed summary, or "" when it is only the method and path the
// catalog fell back to.
func summaryLine(f *Function) string {
	summary := strings.TrimSpace(f.Summary)
	if summary == f.Method+" "+f.Path {
		return ""
	}
	return summary
}

// trimSummary drops a leading repeat of the summary from the description,
// but only at a word boundary: "Get units by id" does not start with the
// summary "Get unit".
func trimSummary(description, summary string) string {
	if summary == "" || !strings.HasPrefix(description, summary) {
		return description
	}
	rest := description[len(summary):]
	if rest == "" {
		return ""
	}
	first, _ := utf8.DecodeRuneInString(rest)
	last, _ := utf8.DecodeLastRuneInString(summary)
	if unicode.IsLetter(first) || unicode.IsDigit(first) {
		if unicode.IsLetter(last) || unicode.IsDigit(last) {
			return description
		}
	}
	return strings.TrimSpace(strings.TrimLeft(rest, " \t\n.:;,"))
}

// moduleNames lists the module names separated by commas.
func (c *Catalog) moduleNames() string {
	names := make([]string, 0, len(c.Modules))
	for _, m := range c.Modules {
		names = append(names, m.Name)
	}
	return strings.Join(names, ", ")
}

// plural formats a count with a noun, adding "s" unless the count is one.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// renderDescribe prints how to call each named function, once per name.
func (c *Catalog) renderDescribe(names []string) string {
	names = uniqueNames(names)
	blocks := make([]string, 0, len(names))
	for _, name := range names {
		blocks = append(blocks, c.describeFunction(name))
	}
	return strings.Join(blocks, "\n\n")
}

// uniqueNames trims each name and drops the repeats, keeping the first.
func uniqueNames(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// describeFunction is the block of one function, or the unknown-name hint.
func (c *Catalog) describeFunction(name string) string {
	f, ok := c.Function(name)
	if !ok {
		return unknownName("function", name, c.functionSuggestions(name))
	}

	header := f.Name + "  " + route(f)
	if f.Deprecated {
		header += "  [deprecated]"
	}
	lines := []string{header}

	summary := summaryLine(f)
	if summary != "" {
		lines = append(lines, "  "+summary)
	}
	lines = append(lines, c.detailLines(f)...)

	if f.Returns != "" && f.ReturnSchema != nil {
		lines = append(lines, "  returns "+f.Returns+": "+c.shape(f.ReturnSchema))
	}
	if f.Returns != "" && f.ReturnSchema == nil {
		lines = append(lines, "  returns "+f.Returns)
	}
	if f.Returns == "" && f.ReturnSchema != nil {
		lines = append(lines, "  returns "+c.shape(f.ReturnSchema))
	}

	// A flattened body leaves no schema of its own in the args, so the terms
	// accepted by f come from the reverse index.
	seen := map[string]bool{}
	for _, t := range c.Terms {
		if slices.Contains(t.AcceptedBy, f.Name) {
			seen[t.Name] = true
		}
	}
	for _, a := range f.Args {
		c.collectTerms(a.Schema, seen)
	}
	c.collectTerms(f.ReturnSchema, seen)
	if f.ReturnSchema != nil {
		for _, key := range f.ReturnSchema.Properties.Keys {
			c.collectTerms(f.ReturnSchema.Properties.Map[key], seen)
		}
	}
	if len(seen) > 0 {
		lines = append(lines, "  see also: "+strings.Join(sortedKeys(seen), ", "))
	}

	return strings.Join(lines, "\n")
}

// route is the method and path of f, or "composite" for a composite function.
func route(f *Function) string {
	if f.Composite {
		return "composite"
	}
	return f.Method + " " + f.Path
}

// detailLines is the description of f without a leading repeat of its
// summary, then its argument table, indented under a function header.
func (c *Catalog) detailLines(f *Function) []string {
	description := trimSummary(strings.TrimSpace(f.Description), summaryLine(f))
	lines := indentLines(description)
	if len(f.Args) > 0 || f.otherArgs {
		lines = append(lines, "  args:")
		lines = append(lines, c.argLines(f)...)
	}
	return lines
}

// argLines prints the arguments of f as aligned columns: name, type, then
// the required flag, enum values and the first line of the description. The
// other keys a body takes come last, as a row named "*" with the type of
// their values.
func (c *Catalog) argLines(f *Function) []string {
	args := f.Args
	if f.otherArgs {
		other := &Arg{Name: "*", In: "body", Description: "other keys, sent in the body", Schema: f.otherSchema}
		args = append(append([]*Arg(nil), args...), other)
	}
	heads := make([]string, len(args))
	enums := make([]string, len(args))
	nameWidth := 0
	typeWidth := 0
	for i, a := range args {
		heads[i], enums[i] = c.argParts(a)
		nameWidth = max(nameWidth, len(a.Name))
		typeWidth = max(typeWidth, len(heads[i]))
	}

	lines := make([]string, 0, len(args))
	for i, a := range args {
		var notes []string
		if a.Required {
			notes = append(notes, "required")
		}
		if enums[i] != "" {
			notes = append(notes, enums[i])
		}
		description := firstLine(a.Description)
		if description != "" {
			notes = append(notes, description)
		}
		line := "    " + pad(a.Name, nameWidth+2) + pad(heads[i], typeWidth+2) + strings.Join(notes, "  ")
		lines = append(lines, strings.TrimRight(line, " "))
	}
	return lines
}

// pad right-pads s with spaces to width.
func pad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// indentLines splits text into non-empty lines indented by two spaces.
func indentLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, "  "+line)
		}
	}
	return lines
}

// functionSuggestions lists up to maxSuggestions function names,
// case-insensitively, each rank in catalog order. For a name with a dot:
// first the one equal to name, then those containing the whole name, then
// those whose function part, after the module and its dot, equals the part
// of name after its first dot, then those containing that part. For a name
// without a dot: first those whose function part equals name, then those
// containing it.
func (c *Catalog) functionSuggestions(name string) []string {
	whole := strings.ToLower(name)
	if whole == "" {
		return nil
	}
	_, after, dotted := strings.Cut(whole, ".")
	part := whole
	if dotted {
		part = after
	}

	var equal, equalPart, containsWhole, containsAfter []string
	for _, f := range c.Functions() {
		lower := strings.ToLower(f.Name)
		_, function, _ := strings.Cut(lower, ".")
		switch {
		case lower == whole:
			equal = append(equal, f.Name)
		case dotted && strings.Contains(lower, whole):
			containsWhole = append(containsWhole, f.Name)
		case function != "" && function == part:
			equalPart = append(equalPart, f.Name)
		case strings.Contains(lower, whole):
			containsWhole = append(containsWhole, f.Name)
		case dotted && after != "" && strings.Contains(lower, after):
			containsAfter = append(containsAfter, f.Name)
		}
	}
	// A name with a dot names a module, so the functions containing the
	// whole of it are closer than the same function part in other modules.
	out := slices.Concat(equal, equalPart, containsWhole, containsAfter)
	if dotted {
		out = slices.Concat(equal, containsWhole, equalPart, containsAfter)
	}
	return out[:min(len(out), maxSuggestions)]
}

// unknownName is the reply for a name that is not in the catalog.
func unknownName(kind, name string, suggestions []string) string {
	out := fmt.Sprintf("unknown %s %q", kind, name)
	if len(suggestions) > 0 {
		out += "\n  did you mean: " + strings.Join(suggestions, ", ")
	}
	return out
}

// renderExplain prints each term with its fields and the functions using it,
// once per name.
func (c *Catalog) renderExplain(terms []string) string {
	terms = uniqueNames(terms)
	blocks := make([]string, 0, len(terms))
	for _, name := range terms {
		blocks = append(blocks, c.explainTerm(name))
	}
	return strings.Join(blocks, "\n\n")
}

// explainTerm is the block of one term, or the unknown-name hint.
func (c *Catalog) explainTerm(name string) string {
	t, ok := c.Term(name)
	if !ok {
		return unknownName("term", name, c.termSuggestions(name))
	}

	header := t.Name
	if len(t.Aliases) > 0 {
		header += "  (aliases: " + strings.Join(t.Aliases, ", ") + ")"
	}
	lines := []string{header}

	description := strings.TrimSpace(t.Description)
	if description == "" {
		lines = append(lines, "  (no description)")
	} else {
		lines = append(lines, indentLines(description)...)
	}

	if t.Schema != nil && t.Schema.Properties.Len() > 0 {
		lines = append(lines, "  fields: "+c.fields(t.Schema))
	}
	if t.Schema != nil && t.Schema.Properties.Len() == 0 {
		lines = append(lines, "  type: "+c.inlineType(t.Schema))
	}
	if len(t.AcceptedBy) > 0 {
		lines = append(lines, "  accepted by: "+strings.Join(t.AcceptedBy, ", "))
	}
	if len(t.ReturnedBy) > 0 {
		lines = append(lines, "  returned by: "+strings.Join(t.ReturnedBy, ", "))
	}
	return strings.Join(lines, "\n")
}

// termSuggestions lists up to maxSuggestions term names whose name or an
// alias contains the given name, case-insensitively.
func (c *Catalog) termSuggestions(name string) []string {
	needle := normalizeTerm(name)
	if needle == "" {
		return nil
	}
	var out []string
	for _, t := range c.Terms {
		if len(out) == maxSuggestions {
			break
		}
		candidates := append([]string{t.Name}, t.Aliases...)
		for _, candidate := range candidates {
			if strings.Contains(normalizeTerm(candidate), needle) {
				out = append(out, t.Name)
				break
			}
		}
	}
	return out
}

// shape prints a schema as its first-level fields, or as its type when it
// has no properties.
func (c *Catalog) shape(s *reference.Schema) string {
	if s.Properties.Len() > 0 {
		return "{" + c.fields(s) + "}"
	}
	return c.inlineType(s)
}

// fields prints the properties of s as "name type" pairs.
func (c *Catalog) fields(s *reference.Schema) string {
	parts := make([]string, 0, s.Properties.Len())
	for _, key := range s.Properties.Keys {
		parts = append(parts, key+" "+c.typeName(s.Properties.Map[key]))
	}
	return strings.Join(parts, ", ")
}

// collectTerms adds the terms s stands for, directly, through array items or
// through oneOf and anyOf members.
func (c *Catalog) collectTerms(s *reference.Schema, seen map[string]bool) {
	if s == nil {
		return
	}
	name := c.termName(s)
	if name != "" {
		_, ok := c.Term(name)
		if ok {
			seen[name] = true
		}
		return
	}
	c.collectTerms(s.Items, seen)
	for _, member := range s.OneOf {
		c.collectTerms(member, seen)
	}
	for _, member := range s.AnyOf {
		c.collectTerms(member, seen)
	}
}

// argType is typeName for an argument.
func (c *Catalog) argType(a *Arg) string {
	head, enum := c.argParts(a)
	if enum == "" {
		return head
	}
	return head + " " + enum
}

// argParts is typeParts for an argument. A path argument is never nullable,
// since dispatch refuses null for it, so its type has no "?" whatever the
// schema says.
func (c *Catalog) argParts(a *Arg) (string, string) {
	head, enum := c.typeParts(a.Schema)
	if a.In == "path" {
		head = strings.TrimSuffix(head, "?")
	}
	return head, enum
}

// typeName is the compact type notation the agent reads: a term name,
// "<items>[]", "<type> <format>", a trailing "?" when nullable, "A|B" for
// alternatives, with an alternative that has an enum in parentheses and
// "(A|B)" before a "?", a "[]" or an enum of its own, "object" for inline
// objects, and ` enum: "a"|"b"` for enums, the values written as JSON, a
// const as a one-value enum. After "<items>[]" the enum is the items'; an
// array's own enum follows "(<items>[])".
func (c *Catalog) typeName(s *reference.Schema) string {
	head, enum := c.typeParts(s)
	if enum == "" {
		return head
	}
	return head + " " + enum
}

// inlineType is typeName without naming s itself after a term, so a term's
// own schema prints as its structure.
func (c *Catalog) inlineType(s *reference.Schema) string {
	head, enum := c.inlineParts(s)
	if enum == "" {
		return head
	}
	return head + " " + enum
}

// typeParts splits typeName into the type and the enum annotation, so the
// describe table can put the enum in its notes column.
func (c *Catalog) typeParts(s *reference.Schema) (string, string) {
	if s == nil {
		return "any", ""
	}
	name := c.termName(s)
	if name != "" {
		return name + nullMark(s), ""
	}
	return c.inlineParts(s)
}

// inlineParts is typeParts for a schema that is not a term.
func (c *Catalog) inlineParts(s *reference.Schema) (string, string) {
	if s == nil {
		return "any", ""
	}
	members := append(append([]*reference.Schema(nil), s.OneOf...), s.AnyOf...)
	if len(members) > 0 {
		names := make([]string, 0, len(members))
		for _, member := range members {
			head, enum := c.typeParts(member)
			// The values of an enum are joined by "|" too, so a member with
			// one is parenthesized.
			if enum != "" {
				names = append(names, "("+head+" "+enum+")")
				continue
			}
			names = append(names, head)
		}
		list := strings.Join(names, "|")
		enum := ownEnum(s)
		// A list of alternatives is wrapped before the "?", and before its
		// own enum, which would otherwise read as the last alternative's.
		if c.listCount(s) > 1 && (s.Nullable || enum != "") {
			list = "(" + list + ")"
		}
		return list + nullMark(s), enum
	}

	if s.Items != nil || s.Type.Primary() == "array" {
		items := s.Items
		head, enum := c.typeParts(items)
		// Items printed as a list of alternatives are wrapped before the
		// "[]", unless nullable or with an enum, as those already are.
		listed := items != nil && c.termName(items) == "" && c.listCount(items) > 1
		if listed && !items.Nullable && enum == "" {
			head = "(" + head + ")"
		}
		own := ownEnum(s)
		if own == "" {
			return head + "[]" + nullMark(s), enum
		}
		// The array's own enum follows the array in parentheses, so it is
		// not read as the items', which then go inside with theirs.
		if enum != "" {
			head = "(" + head + " " + enum + ")"
		}
		return "(" + head + "[])" + nullMark(s), own
	}

	head := s.Type.Primary()
	if head == "" && (s.Properties.Len() > 0 || len(s.AllOf) > 0) {
		head = "object"
	}
	if head == "" {
		head = "any"
	}
	if s.Format != "" {
		head += " " + s.Format
	}
	return head + nullMark(s), ownEnum(s)
}

// ownEnum is the enum annotation of the values s itself allows: its const,
// as a one-value enum, else its enum, else "".
func ownEnum(s *reference.Schema) string {
	if s.Const != nil {
		return enumText([]any{s.Const})
	}
	return enumText(s.Enum)
}

// listCount is the number of alternatives inlineParts joins with "|" for s:
// the members of its oneOf and anyOf, where a member that is itself a list
// counts by its own alternatives unless it prints wrapped, as a nullable one
// or one with an enum of its own does.
func (c *Catalog) listCount(s *reference.Schema) int {
	n := 0
	for _, members := range [][]*reference.Schema{s.OneOf, s.AnyOf} {
		for _, member := range members {
			inner := 0
			if member != nil && c.termName(member) == "" {
				inner = c.listCount(member)
			}
			if inner > 1 && !member.Nullable && ownEnum(member) == "" {
				n += inner
				continue
			}
			n++
		}
	}
	return n
}

// termName returns the term s stands for: the target of its $ref, the member
// of a single-member allOf wrapper, or the term whose schema is s. Refs that
// are not terms return their last path segment; anything else returns "".
func (c *Catalog) termName(s *reference.Schema) string {
	if s.Ref != "" {
		t, ok := c.Term(s.Ref)
		if ok {
			return t.Name
		}
		return s.Ref[strings.LastIndex(s.Ref, "/")+1:]
	}
	if len(s.AllOf) == 1 && s.Properties.Len() == 0 && len(s.Type) == 0 {
		return c.termName(s.AllOf[0])
	}
	for _, t := range c.Terms {
		if t.Schema == s {
			return t.Name
		}
	}
	return ""
}

// nullMark is "?" for nullable schemas.
func nullMark(s *reference.Schema) string {
	if s.Nullable {
		return "?"
	}
	return ""
}

// enumText prints enum values as JSON, `enum: "a"|""|1|null`, or "" without
// values. <, > and & are written as they are, not as the \u escapes
// json.Marshal gives them. A value the encoder refuses is printed with
// fmt.Sprint.
func enumText(values []any) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, v := range values {
		var text strings.Builder
		encoder := json.NewEncoder(&text)
		encoder.SetEscapeHTML(false)
		err := encoder.Encode(v)
		if err != nil {
			parts = append(parts, fmt.Sprint(v))
			continue
		}
		parts = append(parts, strings.TrimSuffix(text.String(), "\n"))
	}
	return "enum: " + strings.Join(parts, "|")
}
