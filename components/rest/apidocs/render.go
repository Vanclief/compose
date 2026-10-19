package apidocs

import (
	"bytes"
	"embed"
	"encoding/json"
	"html"
	"html/template"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vanclief/ez"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets/apidocs.css assets/apidocs.js
var assetFS embed.FS

var pageTemplate = template.Must(template.New("apidocs").Funcs(template.FuncMap{
	// href links to a page anchor. Anchors only hold [A-Za-z0-9._/-], so
	// marking them as a URL is safe and keeps the slash unescaped.
	"href": func(anchor string) template.URL {
		return template.URL("#" + anchor)
	},
}).ParseFS(templateFS, "templates/*.html"))

// Options configures Render and New.
type Options struct {
	// Title is the <title> of the page; defaults to info.title.
	Title string
	// AssetsURL is the base URL serving apidocs.css and apidocs.js (see
	// Assets). Empty inlines both into the page, which then needs no other
	// request but also needs a CSP that allows inline scripts.
	AssetsURL string
	// SpecURL is an optional link to the raw spec shown in the header; empty
	// hides it.
	SpecURL string
	// ServerURL is the base URL for curl samples and the try-it client;
	// empty uses servers[0], then the page origin.
	ServerURL string
}

type pageData struct {
	*pageView
	Inline bool
}

// Render renders doc as a complete HTML page.
func Render(doc *Document, opts Options) ([]byte, error) {
	if doc == nil {
		return nil, ez.New(ez.EINVALID, "apidocs: nil document", nil)
	}

	data := pageData{pageView: buildPage(doc, opts), Inline: opts.AssetsURL == ""}
	if data.Inline {
		data.CSS = template.CSS(embeddedAssets["apidocs.css"].data)
		data.JS = template.JS(embeddedAssets["apidocs.js"].data)
	}

	var buf bytes.Buffer
	err := pageTemplate.ExecuteTemplate(&buf, "page.html", data)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "apidocs: render page", err)
	}

	return buf.Bytes(), nil
}

// maxTreeDepth bounds schema nesting for pathological documents; deeper
// levels render as a type label only.
const maxTreeDepth = 12

// tree writes a schema as nested, collapsible rows.
//
// The root schema and inline (anonymous) objects are written in full. A row
// whose type is a named component is written empty with data-model pointing
// at that model's card; the browser clones the card's tree into it when the
// row is expanded (or, for first-level rows that start open, when it nears
// the viewport). Every component is therefore written exactly once, in its
// card, and the same model still expands in every operation that uses it.
//
// Recursion is cut per ancestor chain: a named schema that already appears
// above the current row renders as a "recursive" link to its card.
type tree struct {
	b     *builder
	d     *Document
	sb    strings.Builder
	chain []string
	mode  ExampleMode
	// filter drops readOnly properties from request trees and writeOnly
	// properties from response trees (and hides them in cloned model trees
	// through the for-request and for-response classes).
	filter bool
}

func (b *builder) newTree(mode ExampleMode, filter bool) *tree {
	return &tree{b: b, d: b.d, mode: mode, filter: filter}
}

func (t *tree) String() string {
	return t.sb.String()
}

func (t *tree) w(parts ...string) {
	for _, p := range parts {
		t.sb.WriteString(p)
	}
}

func (t *tree) inChain(name string) bool {
	for _, n := range t.chain {
		if n == name {
			return true
		}
	}
	return false
}

func (t *tree) push(name string) bool {
	if name == "" {
		return false
	}
	t.chain = append(t.chain, name)
	return true
}

func (t *tree) pop(pushed bool) {
	if pushed {
		t.chain = t.chain[:len(t.chain)-1]
	}
}

var esc = template.HTMLEscapeString

// root renders a body, response or model schema. self names the schema when
// it is a component rendered directly (a model card).
func (t *tree) root(s *Schema, self string) template.HTML {
	if s == nil {
		return ""
	}
	_, name := t.d.Schema(s)
	if self != "" {
		name = self
	}
	f := t.d.Flatten(s)
	if f == nil {
		return ""
	}

	pushed := t.push(name)
	switch {
	case !t.filter:
		t.w(`<div class="tree">`)
	case t.mode == ForRequest:
		t.w(`<div class="tree for-request">`)
	default:
		t.w(`<div class="tree for-response">`)
	}
	desc := s.Description
	if desc == "" {
		desc = f.Description
	}
	if desc != "" {
		t.w(`<div class="tree-desc md">`, string(markdown(desc)), `</div>`)
	}
	t.content(f, name, 0)
	t.w(`</div>`)
	t.pop(pushed)

	return template.HTML(t.String())
}

func isArray(f *Schema) bool {
	return f.Type.Primary() == "array" || (len(f.Type) == 0 && f.Items != nil)
}

func isObject(f *Schema) bool {
	return f.Type.Primary() == "object" || f.Properties.Len() > 0 || f.AdditionalSchema() != nil
}

func (t *tree) hasVariants(f *Schema) bool {
	return (len(f.OneOf) > 0 && !t.bareRequired(f.OneOf)) || (len(f.AnyOf) > 0 && !t.bareRequired(f.AnyOf))
}

func (t *tree) hasNotes(f *Schema) bool {
	return (len(f.OneOf) > 0 && t.bareRequired(f.OneOf)) || (len(f.AnyOf) > 0 && t.bareRequired(f.AnyOf)) || f.Not != nil
}

// expandable reports whether a row for f has anything to show underneath.
func (t *tree) expandable(f *Schema) bool {
	if f == nil {
		return false
	}
	return t.hasVariants(f) || f.Properties.Len() > 0 || f.AdditionalSchema() != nil || t.hasNotes(f)
}

// bareRequired reports whether every member only lists required properties,
// the pattern generators use for "one of these fields must be present".
func (t *tree) bareRequired(list []*Schema) bool {
	if len(list) == 0 {
		return false
	}
	for _, m := range list {
		r, _ := t.d.Schema(m)
		if r == nil || len(r.Required) == 0 {
			return false
		}
		if len(r.Type) > 0 || r.Properties.Len() > 0 || r.Items != nil || len(r.Enum) > 0 ||
			len(r.AllOf) > 0 || len(r.OneOf) > 0 || len(r.AnyOf) > 0 || r.Not != nil {
			return false
		}
	}
	return true
}

func (t *tree) content(f *Schema, name string, depth int) {
	if t.hasVariants(f) {
		t.variants(f, depth)
		return
	}
	if isObject(f) {
		t.object(f, depth)
		return
	}

	t.w(`<div class="tree-head"><span class="ty">`, t.labelOf(f, name, 0), `</span>`, t.markers(f, f, false), `</div>`)
	t.meta(f, nil)
	t.notes(f)

	if !isArray(f) || f.Items == nil {
		return
	}
	_, itemName := t.d.Schema(f.Items)
	items := t.d.Flatten(f.Items)
	if !t.expandable(items) || depth >= maxTreeDepth {
		return
	}
	if itemName != "" && t.inChain(itemName) {
		t.w(`<div class="note">Items are `, t.refLink(itemName), ` (recursive).</div>`)
		return
	}
	pushed := t.push(itemName)
	t.w(`<div class="tree-items">`)
	t.content(items, itemName, depth)
	t.w(`</div>`)
	t.pop(pushed)
}

func (t *tree) object(f *Schema, depth int) {
	for _, key := range f.Properties.Keys {
		t.row(rowSpec{name: key, schema: f.Properties.Map[key], required: f.IsRequired(key)}, depth)
	}

	extra := f.AdditionalSchema()
	if extra != nil {
		t.row(rowSpec{key: true, schema: extra}, depth)
	}

	if f.Properties.Len() == 0 && extra == nil && !t.hasNotes(f) {
		closed := f.AdditionalProperties != nil && !f.AdditionalProperties.Allowed
		if f.Properties.Map != nil || closed {
			t.w(`<div class="note">No properties.</div>`)
		} else {
			t.w(`<div class="note">Free-form object: any properties are accepted.</div>`)
		}
	}

	t.notes(f)
}

func (t *tree) requiredList(list []*Schema) string {
	var parts []string
	for _, m := range list {
		r, _ := t.d.Schema(m)
		var names []string
		for _, n := range r.Required {
			names = append(names, `<code>`+esc(n)+`</code>`)
		}
		if len(names) == 1 {
			parts = append(parts, names[0])
		} else {
			parts = append(parts, "("+strings.Join(names, " and ")+")")
		}
	}
	return strings.Join(parts, ", ")
}

func (t *tree) notes(f *Schema) {
	if len(f.AnyOf) > 0 && t.bareRequired(f.AnyOf) {
		t.w(`<div class="note">At least one of `, t.requiredList(f.AnyOf), ` is required.</div>`)
	}
	if len(f.OneOf) > 0 && t.bareRequired(f.OneOf) {
		t.w(`<div class="note">Exactly one of `, t.requiredList(f.OneOf), ` is required.</div>`)
	}
	if f.Not == nil {
		return
	}
	n := t.d.Flatten(f.Not)
	switch {
	case n == nil:
	case t.bareRequired([]*Schema{f.Not}):
		var names []string
		for _, r := range n.Required {
			names = append(names, `<code>`+esc(r)+`</code>`)
		}
		t.w(`<div class="note">Must not include `, strings.Join(names, ", "), `.</div>`)
	case len(n.Enum) > 0:
		t.w(`<div class="note">Must not be one of `, chips(n.Enum), `.</div>`)
	default:
		t.w(`<div class="note">Must not match `, t.label(f.Not), `.</div>`)
	}
}

func (t *tree) variants(f *Schema, depth int) {
	list := f.OneOf
	kind := "One of"
	base := *f
	if len(f.OneOf) > 0 && !t.bareRequired(f.OneOf) {
		base.OneOf = nil
	} else {
		list = f.AnyOf
		kind = "Any of"
		base.AnyOf = nil
	}

	t.w(`<div class="variants tabset"><div class="variants-head"><span class="ml">`, kind, `</span><div class="tablist" role="tablist">`)
	for i, v := range list {
		t.w(`<button type="button" role="tab" class="tab" aria-selected="`, strconv.FormatBool(i == 0), `">`, esc(t.variantLabel(f, v, i)), `</button>`)
	}
	t.w(`</div></div>`)

	for i, v := range list {
		if i == 0 {
			t.w(`<div class="tabpanel" role="tabpanel">`)
		} else {
			t.w(`<div class="tabpanel" role="tabpanel" hidden>`)
		}

		_, name := t.d.Schema(v)
		fv := t.d.Flatten(v)
		switch {
		case fv == nil:
		case name != "" && t.inChain(name):
			t.w(`<div class="note">`, t.refLink(name), ` (recursive).</div>`)
		case name != "" && base.Properties.Len() == 0 && t.b.models[name] != "":
			// A named variant is its model card's tree, cloned in the browser.
			t.w(`<div class="tree-head"><span class="ty">`, t.labelOf(fv, name, 0), `</span></div>`)
			t.w(`<div class="variant-body" data-model="`, esc(t.b.models[name]), `" data-autofill></div>`)
		default:
			if fv.Description != "" {
				t.w(`<div class="tree-desc md">`, string(markdown(fv.Description)), `</div>`)
			}
			pushed := t.push(name)
			t.content(mergeVariant(&base, fv), name, depth)
			t.pop(pushed)
		}
		t.w(`</div>`)
	}
	t.w(`</div>`)
}

// mergeVariant overlays a oneOf/anyOf member on the properties of the schema
// that holds it, the way Redoc shows each variant as a complete object.
func mergeVariant(base, v *Schema) *Schema {
	if base.Properties.Len() == 0 && len(base.Required) == 0 && len(base.AnyOf) == 0 && len(base.OneOf) == 0 && base.Not == nil {
		return v
	}

	out := *v
	out.Properties = Ordered[*Schema]{}
	for _, key := range base.Properties.Keys {
		out.Properties.Set(key, base.Properties.Map[key])
	}
	for _, key := range v.Properties.Keys {
		out.Properties.Set(key, v.Properties.Map[key])
	}
	out.Required = append([]string(nil), base.Required...)
	for _, r := range v.Required {
		if !out.IsRequired(r) {
			out.Required = append(out.Required, r)
		}
	}
	if len(out.Type) == 0 {
		out.Type = base.Type
	}
	if len(out.AnyOf) == 0 {
		out.AnyOf = base.AnyOf
	}
	if len(out.OneOf) == 0 {
		out.OneOf = base.OneOf
	}
	if out.Not == nil {
		out.Not = base.Not
	}
	if out.AdditionalProperties == nil {
		out.AdditionalProperties = base.AdditionalProperties
	}
	return &out
}

func (t *tree) variantLabel(parent, v *Schema, i int) string {
	_, name := t.d.Schema(v)
	disc := parent.Discriminator
	if disc != nil && name != "" {
		keys := make([]string, 0, len(disc.Mapping))
		for k := range disc.Mapping {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if RefName(disc.Mapping[k], refSchemas) == name {
				return k
			}
		}
	}
	if name != "" {
		short, ok := t.b.short[name]
		if ok {
			return short
		}
		return name
	}

	fv := t.d.Flatten(v)
	if fv == nil {
		return "Variant " + strconv.Itoa(i+1)
	}
	if fv.Title != "" {
		return fv.Title
	}

	// A property pinned to one value (scope: complex) names the variant well.
	keys := fv.Properties.Keys
	if disc != nil && disc.PropertyName != "" {
		keys = append([]string{disc.PropertyName}, keys...)
	}
	for _, key := range keys {
		p, ok := fv.Properties.Get(key)
		if !ok {
			continue
		}
		pf := t.d.Flatten(p)
		if pf == nil {
			continue
		}
		if len(pf.Enum) == 1 {
			return key + ": " + scalarString(pf.Enum[0])
		}
		if pf.Const != nil {
			return key + ": " + scalarString(pf.Const)
		}
	}

	if !isObject(fv) && len(fv.Type) > 0 {
		return t.plainLabel(v)
	}
	return "Variant " + strconv.Itoa(i+1)
}

type rowSpec struct {
	name        string
	key         bool // an additionalProperties row, shown as <key>
	schema      *Schema
	required    bool
	description string
	deprecated  bool
	example     any
	param       bool
}

func (t *tree) paramRow(name string, s *Schema, description string, required, deprecated bool, example any) {
	t.row(rowSpec{name: name, schema: s, required: required, description: description, deprecated: deprecated, example: example, param: true}, 0)
}

func (t *tree) row(r rowSpec, depth int) {
	s := r.schema
	refName := ""
	var f *Schema
	if s != nil {
		_, refName = t.d.Schema(s)
		f = t.d.Flatten(s)
	}
	if f == nil {
		f = &Schema{}
		s = f
	}

	if t.filter && !r.param {
		if t.mode == ForRequest && (s.ReadOnly || f.ReadOnly) {
			return
		}
		if t.mode == ForResponse && (s.WriteOnly || f.WriteOnly) {
			return
		}
	}

	desc := r.description
	if desc == "" {
		desc = s.Description
	}
	if desc == "" {
		desc = f.Description
	}

	kid := f
	kidName := refName
	for i := 0; i < 8 && isArray(kid) && kid.Items != nil; i++ {
		_, kidName = t.d.Schema(kid.Items)
		kid = t.d.Flatten(kid.Items)
	}
	hasKids := t.expandable(kid)
	recursive := hasKids && kidName != "" && t.inChain(kidName)
	if recursive || depth >= maxTreeDepth {
		hasKids = false
	}

	// Named schemas are never written twice: a row that references one
	// expands by cloning that model card's tree in the browser. Rows on the
	// first level start open and are filled as they scroll into view. Writing
	// every reference inline made emma's page 16 MB.
	lazy := ""
	if hasKids && kidName != "" {
		anchor, ok := t.b.models[kidName]
		if ok {
			lazy = anchor
		} else {
			hasKids = false
		}
	}
	open := hasKids && depth < 1

	name := esc(r.name)
	if r.key {
		name = `<span class="pk">&lt;key&gt;</span>`
	}

	rowClass := "row"
	if s.ReadOnly || f.ReadOnly {
		rowClass += " ro"
	}
	if s.WriteOnly || f.WriteOnly {
		rowClass += " wo"
	}

	t.w(`<div class="`, rowClass, `"><div class="row-head">`)
	switch {
	case open:
		t.w(`<button type="button" class="tg pn" aria-expanded="true">`, name, `</button>`)
	case hasKids:
		t.w(`<button type="button" class="tg pn" aria-expanded="false">`, name, `</button>`)
	default:
		t.w(`<span class="pn">`, name, `</span>`)
	}
	t.w(`<span class="ty">`, t.labelOf(f, refName, 0), `</span>`)
	if r.required {
		t.w(`<span class="mk req">required</span>`)
	}
	t.w(t.markers(s, f, r.deprecated))
	if recursive {
		anchor, ok := t.b.models[kidName]
		if ok {
			t.w(`<a class="mk rec" href="#`, esc(anchor), `">recursive</a>`)
		} else {
			t.w(`<span class="mk rec">recursive</span>`)
		}
	}
	t.w(`</div>`)

	if desc != "" {
		t.w(`<div class="row-desc md">`, string(markdown(desc)), `</div>`)
	}
	t.meta(f, r.example)

	switch {
	case lazy != "" && open:
		t.w(`<div class="row-kids" data-model="`, esc(lazy), `" data-autofill></div>`)
	case lazy != "":
		t.w(`<div class="row-kids" hidden data-model="`, esc(lazy), `"></div>`)
	case hasKids:
		if open {
			t.w(`<div class="row-kids">`)
		} else {
			t.w(`<div class="row-kids" hidden>`)
		}
		pushed := t.push(kidName)
		t.content(kid, kidName, depth+1)
		t.pop(pushed)
		t.w(`</div>`)
	}
	t.w(`</div>`)
}

func (t *tree) markers(s, f *Schema, deprecated bool) string {
	var sb strings.Builder
	if s.Nullable || f.Nullable {
		sb.WriteString(`<span class="mk nul">nullable</span>`)
	}
	if s.ReadOnly || f.ReadOnly {
		sb.WriteString(`<span class="mk ro">read-only</span>`)
	}
	if s.WriteOnly || f.WriteOnly {
		sb.WriteString(`<span class="mk wo">write-only</span>`)
	}
	if deprecated || s.Deprecated || f.Deprecated {
		sb.WriteString(`<span class="mk dep">deprecated</span>`)
	}
	return sb.String()
}

func (t *tree) refLink(name string) string {
	anchor, ok := t.b.models[name]
	if !ok {
		return `<span class="ref">` + esc(name) + `</span>`
	}
	return `<a class="ref" href="#` + esc(anchor) + `" title="` + esc(name) + `">` + esc(t.b.short[name]) + `</a>`
}

// label is the type label of s, keeping the component name of a $ref.
func (t *tree) label(s *Schema) string {
	if s == nil {
		return "any"
	}
	_, name := t.d.Schema(s)
	f := t.d.Flatten(s)
	if f == nil {
		return "any"
	}
	return t.labelOf(f, name, 0)
}

var tagStrip = regexp.MustCompile(`<[^>]*>`)

// plainLabel is label without markup, for placeholders and tab captions.
func (t *tree) plainLabel(s *Schema) string {
	return html.UnescapeString(tagStrip.ReplaceAllString(t.label(s), ""))
}

// labelOf builds labels such as "string <uuid>", "array of integer <int64>"
// and "object (StandardError)" for flattened schema f named name.
func (t *tree) labelOf(f *Schema, name string, depth int) string {
	ref := ""
	if name != "" {
		ref = " (" + t.refLink(name) + ")"
	}

	var types []string
	for _, ty := range f.Type {
		if ty != "null" {
			types = append(types, ty)
		}
	}
	ty := ""
	if len(types) == 1 {
		ty = types[0]
	}
	if len(types) == 0 {
		switch {
		case isObject(f):
			ty = "object"
		case f.Items != nil:
			ty = "array"
		case f.Const != nil:
			ty = jsonType(f.Const)
		case len(f.Enum) > 0:
			ty = jsonType(f.Enum[0])
		}
	}

	switch {
	case len(types) > 1:
		return esc(strings.Join(types, " | ")) + ref
	case ty == "array":
		if f.Items == nil || depth > 8 {
			return "array" + ref
		}
		_, itemName := t.d.Schema(f.Items)
		items := t.d.Flatten(f.Items)
		if items == nil {
			return "array" + ref
		}
		return "array of " + t.labelOf(items, itemName, depth+1)
	case ty == "object":
		if name != "" {
			return "object" + ref
		}
		if f.Properties.Map == nil && f.AdditionalSchema() == nil && !t.hasVariants(f) && !t.hasNotes(f) {
			return "object (any)"
		}
		return "object"
	case ty == "" && len(f.OneOf) > 0 && !t.bareRequired(f.OneOf):
		return "one of" + ref
	case ty == "" && len(f.AnyOf) > 0 && !t.bareRequired(f.AnyOf):
		return "any of" + ref
	case ty == "":
		return "any" + ref
	}

	out := esc(ty)
	if f.Format != "" {
		out += " &lt;" + esc(f.Format) + "&gt;"
	}
	return out + ref
}

// jsonType names the JSON type of a decoded value, for untyped const and
// enum schemas.
func jsonType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case int, int64, uint64:
		return "integer"
	case float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return ""
}

func chips(values []any) string {
	var sb strings.Builder
	for _, v := range values {
		sb.WriteString(`<code class="chip">`)
		if v == nil {
			sb.WriteString("null")
		} else if v == "" {
			sb.WriteString(`""`)
		} else {
			sb.WriteString(esc(scalarString(v)))
		}
		sb.WriteString(`</code>`)
	}
	return sb.String()
}

// jsonValue renders a default, constant or example inline, truncated.
func jsonValue(v any) string {
	data, err := json.Marshal(v)
	out := string(data)
	if err != nil {
		out = scalarString(v)
	}
	runes := []rune(out)
	if len(runes) > 160 {
		out = string(runes[:160]) + "…"
	}
	return out
}

func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// constraints describes numeric, length, item and property bounds.
func constraints(f *Schema) []string {
	var out []string

	lo := ""
	loOpen := false
	if f.Minimum != nil {
		lo = formatNumber(*f.Minimum)
	}
	if f.ExclusiveMinimum != nil && f.ExclusiveMinimum.Value != nil {
		lo = formatNumber(*f.ExclusiveMinimum.Value)
		loOpen = true
	}
	hi := ""
	hiOpen := false
	if f.Maximum != nil {
		hi = formatNumber(*f.Maximum)
	}
	if f.ExclusiveMaximum != nil && f.ExclusiveMaximum.Value != nil {
		hi = formatNumber(*f.ExclusiveMaximum.Value)
		hiOpen = true
	}
	switch {
	case lo != "" && hi != "":
		left := "["
		if loOpen {
			left = "("
		}
		right := "]"
		if hiOpen {
			right = ")"
		}
		out = append(out, left+lo+" .. "+hi+right)
	case lo != "" && loOpen:
		out = append(out, "> "+lo)
	case lo != "":
		out = append(out, ">= "+lo)
	case hi != "" && hiOpen:
		out = append(out, "< "+hi)
	case hi != "":
		out = append(out, "<= "+hi)
	}
	if f.MultipleOf != nil {
		out = append(out, "multiple of "+formatNumber(*f.MultipleOf))
	}

	out = append(out, rangeText(f.MinLength, f.MaxLength, "characters", "non-empty")...)
	out = append(out, rangeText(f.MinItems, f.MaxItems, "items", "non-empty")...)
	if f.UniqueItems {
		out = append(out, "unique items")
	}
	out = append(out, rangeText(f.MinProperties, f.MaxProperties, "properties", "non-empty")...)

	return out
}

func rangeText(lo, hi *int, unit, nonEmpty string) []string {
	switch {
	case lo != nil && hi != nil && *lo == *hi:
		return []string{strconv.Itoa(*lo) + " " + unit}
	case lo != nil && hi != nil:
		return []string{"[" + strconv.Itoa(*lo) + " .. " + strconv.Itoa(*hi) + "] " + unit}
	case lo != nil && *lo == 1:
		return []string{nonEmpty}
	case lo != nil && *lo > 0:
		return []string{">= " + strconv.Itoa(*lo) + " " + unit}
	case hi != nil:
		return []string{"<= " + strconv.Itoa(*hi) + " " + unit}
	}
	return nil
}

func (t *tree) meta(f *Schema, example any) {
	var parts []string

	enum := f.Enum
	enumLabel := "Enum"
	if len(enum) == 0 && isArray(f) && f.Items != nil {
		items := t.d.Flatten(f.Items)
		if items != nil && len(items.Enum) > 0 {
			enum = items.Enum
			enumLabel = "Items"
		}
	}
	if len(enum) > 0 {
		parts = append(parts, `<span class="mi"><span class="ml">`+enumLabel+`</span>`+chips(enum)+`</span>`)
	}
	if f.Const != nil {
		parts = append(parts, `<span class="mi"><span class="ml">Constant</span><code>`+esc(jsonValue(f.Const))+`</code></span>`)
	}
	if f.Default != nil {
		parts = append(parts, `<span class="mi"><span class="ml">Default</span><code>`+esc(jsonValue(f.Default))+`</code></span>`)
	}
	for _, c := range constraints(f) {
		parts = append(parts, `<span class="cn">`+esc(c)+`</span>`)
	}
	if f.Pattern != "" {
		parts = append(parts, `<span class="mi"><span class="ml">Pattern</span><code>`+esc(f.Pattern)+`</code></span>`)
	}

	if example == nil {
		example = f.Example
	}
	if example == nil && len(f.Examples) > 0 {
		example = f.Examples[0]
	}
	if example != nil {
		parts = append(parts, `<span class="mi"><span class="ml">Example</span><code>`+esc(jsonValue(example))+`</code></span>`)
	}

	if len(parts) > 0 {
		t.w(`<div class="row-meta">`, strings.Join(parts, ""), `</div>`)
	}
}
