package reference

import (
	"bytes"
	"encoding/json"
	"html/template"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The view model is computed once in Go so the templates stay dumb: every
// label, anchor, markdown fragment, schema tree and example is final here.

type pageView struct {
	Title       string
	InfoTitle   string
	Version     string
	OpenAPI     string
	Summary     string
	Description template.HTML
	Terms       string
	Contact     *Contact
	License     *License
	Servers     []serverView
	ServerURL   string
	SpecURL     string
	Schemes     []*schemeView
	Global      []requirementView
	Groups      []*groupView
	HasInternal bool // some operation is x-internal: the sidebar offers Show internal
	Models      []*modelView
	ModelNav    modelNav
	Map         *mapView
	Diff        *changesView
	AssetsURL   string
	CSSVersion  string
	JSVersion   string
	CSS         template.CSS
	JS          template.JS
	Flow        template.JS
	TypeNotes   template.JS // JSON list of sample type descriptions, see typesRef
	// ModelSamples maps each model's anchor to its example and type notes
	// for the model dialog, see modelSamplesJSON.
	ModelSamples template.JS
	// ModelDiffs is the side-by-side diff of the changed models in diff
	// mode, see modelDiffs.
	ModelDiffs template.JS
	// SampleDiffs is the diff of the changed response samples in diff mode,
	// see sampleDiffs.
	SampleDiffs template.JS
}

// serverView is one server: URL has every variable replaced by its default
// and is what curl and try-it use; Template is the raw URL when it differs.
type serverView struct {
	URL         string
	Template    string
	Variables   []serverVariableView
	Description string
	Selected    bool
}

type serverVariableView struct {
	Name        string
	Default     string
	Enum        string
	Description string
}

type schemeView struct {
	Name        string
	Anchor      string
	Kind        string
	Detail      template.HTML
	Description template.HTML
	Global      bool
	Inputs      []authInput
}

// authInput is one credential field of the try-it Auth panel.
type authInput struct {
	Key         string // sessionStorage key and data-auth value
	Scheme      string
	Label       string
	Kind        string // apiKey, bearer, basic-user, basic-pass
	In          string // header, query or cookie for apiKey
	Param       string // header or query name for apiKey
	Placeholder string
	Secret      bool
}

type requirementView struct {
	Schemes []schemeRef
}

type schemeRef struct {
	Name   string
	Anchor string
	Scopes string
}

// groupView is the operations under one first path segment, /complexes or
// /users, in document order: one sidebar group and one page section.
type groupView struct {
	Name        string
	Anchor      string
	Description template.HTML
	Ops         []*opView
	NavTree     *navFolder
}

// navFolder is one level of a group's sidebar tree: operations first, in
// document order, then sub-folders by name. Count includes every operation
// beneath the folder.
type navFolder struct {
	Name    string
	Count   int
	Ops     []*opView
	Folders []*navFolder
	scope   bool
}

// navFlatMax is the largest group that is always listed flat: a handful of
// operations reads better without folders.
const navFlatMax = 8

type opView struct {
	Anchor      string
	Method      string
	MethodClass string
	Path        string
	Crumbs      []crumbView
	Title       string
	OperationID string
	Deprecated  bool
	Internal    bool
	Description template.HTML
	Security    []requirementView
	NoAuth      bool
	SecurityKey string // JSON list of requirement scheme lists, read by the try-it client
	ParamGroups []paramGroup
	Body        *bodyView
	CurlAuth    string        // JSON credential args for the copied curl, see curlAuth
	ReqExample  template.HTML // the request body example the body port's fields start from
	ReqTypes    string        // index of the request example's type description, see typesRef
	ResSamples  []sampleView
	Search      string
	TryParams   []tryParam
	TryBody     bool
	TryCT       string
	TryServer   string // the operation's own server, when it overrides servers[0]
	Machine     machineView
	Trace       string // trace ids of the models the operation produces or consumes
	ref         OperationRef
	Diff        *diffBadge
}

// paramGroup holds the parameter rows of one location, required ones first.
type paramGroup struct {
	In       string
	Rows     template.HTML
	Optional template.HTML
}

type bodyView struct {
	Required    bool
	Description template.HTML
	Contents    []contentView
	Summary     summaryView
}

// summaryView is the one-line shape of a body or response: the model it
// carries, a list of models, or a plain type label.
type summaryView struct {
	Type template.HTML
}

type contentView struct {
	Type string
	Tree template.HTML
}

type sampleView struct {
	Label   string
	Class   string
	Type    string
	Model   *sampleModelView // the named schema the sample shows, if any
	Example template.HTML
	Types   string // index of the example's type description, see typesRef
	// In diff mode: the sample's badge, its index in the sample diffs (see
	// sampleDiffs) and whether the head document no longer has it.
	Diff    *diffBadge
	DiffRef string
	Removed bool
}

// sampleModelView names the model a sample shows, next to its content
// type, linked to the model like the inspector's type labels: Name, Name[]
// for a list of them, after the muted package Prefix that tells apart models
// sharing a name. Class is the inspector's type class.
type sampleModelView struct {
	Anchor string
	Prefix string
	Name   string
	Title  string
	Class  string
	Diff   *diffBadge
}

// textEscaper escapes text for element content only. html/template's escaper
// also turns every quote into &#34;, which grows JSON samples by a third.
var textEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func textHTML(s string) template.HTML {
	return template.HTML(textEscaper.Replace(s))
}

// tryParam is one try-it input. Array values are typed comma separated;
// Explode says whether the client sends them as repeated keys (a=1&a=2),
// otherwise they travel as one value joined with Sep.
type tryParam struct {
	Name        string
	In          string
	Required    bool
	Array       bool
	Explode     bool
	Sep         string
	Placeholder string
}

// modelView is one model of the Models view. Its fields show in an
// ObjectInspector built from its sample (see addModelSample) when it has
// one, and in Tree, the full schema with every constraint. In diff mode a
// changed, added or removed model opens on its changes (see modelDiffs).
// Table is the
// database table it is stored in (see Document.Table).
type modelView struct {
	Name        string
	Short       string
	Anchor      string
	Table       string
	Description template.HTML
	Tree        template.HTML
	Inspector   bool
	Search      string
	MadeBy      int
	UsedBy      int
	Trace       string
	Diff        *diffBadge
}

// modelNav is the Models sidebar: a folder per operation tag (see
// tagGroups), or, in a document without tags, models whose generated names
// carry a module (see schemaGroup) in a folder per module and the rest loose
// above them. Tables counts the models stored in a database table.
type modelNav struct {
	Loose  []*modelView
	Groups []modelGroup
	Tables int
}

// order lists the models as the sidebar does: loose ones first, then each
// group's.
func (nav modelNav) order() []*modelView {
	out := append([]*modelView{}, nav.Loose...)
	for _, g := range nav.Groups {
		out = append(out, g.Models...)
	}
	return out
}

type builder struct {
	d       *Document
	opts    Options
	used    map[string]bool
	short   map[string]string // component schema name to display name
	models  map[string]string // component schema name to anchor
	schemes map[string]string // security scheme name to anchor
	server  string
	flow    *flowIndex
	diff    *diffState // nil unless Options.Base is set
	// notes holds each distinct sample type description once; samples
	// point at theirs by index, see typesRef.
	notes     []string
	noteIndex map[string]int
	samples   map[string]modelSample
}

func buildPage(d *Document, opts Options, diff *diffState) *pageView {
	b := &builder{
		d:       d,
		opts:    opts,
		used:    map[string]bool{},
		models:  map[string]string{},
		schemes: map[string]string{},
		diff:    diff,

		noteIndex: map[string]int{},
		samples:   map[string]modelSample{},
	}

	names := d.Components.Schemas.Keys
	if diff != nil {
		names = append(append([]string(nil), names...), diff.removedModels(d)...)
	}
	b.short = shortNames(names)
	for _, name := range names {
		b.models[name] = b.anchor("model", modelSlug(name))
	}

	b.server = opts.ServerURL
	if b.server == "" && len(d.Servers) > 0 {
		b.server = d.Servers[0].Expand()
	}

	p := &pageView{
		Title:       opts.Title,
		InfoTitle:   d.Info.Title,
		Version:     d.Info.Version,
		OpenAPI:     d.OpenAPI,
		Summary:     d.Info.Summary,
		Description: markdown(d.Info.Description),
		Terms:       d.Info.TermsOfService,
		Contact:     d.Info.Contact,
		License:     d.Info.License,
		ServerURL:   b.server,
		SpecURL:     opts.SpecURL,
		AssetsURL:   strings.TrimSuffix(opts.AssetsURL, "/"),
		CSSVersion:  assetVersion("reference.css"),
		JSVersion:   assetVersion("reference.js"),
	}
	if p.Title == "" {
		p.Title = d.Info.Title
	}
	if p.Title == "" {
		p.Title = "API Reference"
	}
	if p.InfoTitle == "" {
		p.InfoTitle = p.Title
	}

	for _, s := range d.Servers {
		p.Servers = append(p.Servers, newServerView(s, b.server))
	}
	if opts.ServerURL != "" && !hasServer(d.Servers, opts.ServerURL) {
		p.Servers = append([]serverView{{URL: opts.ServerURL, Selected: true}}, p.Servers...)
	}

	p.Schemes = b.buildSchemes()
	p.Global = b.requirements(d.Security)
	p.Groups = b.buildGroups()
	for _, g := range p.Groups {
		for _, ov := range g.Ops {
			p.HasInternal = p.HasInternal || ov.Internal
		}
	}
	p.Models = b.buildModels()
	p.ModelNav = b.buildModelNav(p.Models)
	p.Models = p.ModelNav.order()
	p.Flow = b.flow.JSON
	p.ModelDiffs = b.modelDiffsJSON()
	p.SampleDiffs = b.sampleDiffsJSON()
	p.ModelSamples = b.modelSamplesJSON()
	p.TypeNotes = b.typeNotesJSON()
	p.Map = b.buildMap()
	p.Diff = b.changesView(p)

	return p
}

func hasServer(servers []Server, url string) bool {
	for _, s := range servers {
		if s.Expand() == url {
			return true
		}
	}
	return false
}

func newServerView(s Server, selected string) serverView {
	v := serverView{URL: s.Expand(), Description: s.Description}
	v.Selected = v.URL == selected
	if v.URL == s.URL {
		return v
	}
	v.Template = s.URL
	names := make([]string, 0, len(s.Variables))
	for name := range s.Variables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sv := s.Variables[name]
		v.Variables = append(v.Variables, serverVariableView{
			Name:        name,
			Default:     sv.Default,
			Enum:        strings.Join(sv.Enum, ", "),
			Description: sv.Description,
		})
	}
	return v
}

// anchor returns a page-unique id, suffixing -2, -3 on collision.
func (b *builder) anchor(prefix, slug string) string {
	id := prefix + "/" + slug
	candidate := id
	for n := 2; b.used[candidate]; n++ {
		candidate = id + "-" + strconv.Itoa(n)
	}
	b.used[candidate] = true
	return candidate
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// slugify lowercases s and turns every run of other characters into a dash:
// visitors.getAccessCodePublic becomes visitors-getaccesscodepublic.
func slugify(s string) string {
	out := strings.Trim(slugStrip.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if out == "" {
		return "item"
	}
	return out
}

var modelStrip = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// modelSlug keeps the component name readable (and case) in #model/<name>.
func modelSlug(name string) string {
	out := strings.Trim(modelStrip.ReplaceAllString(name, "-"), "-")
	if out == "" {
		return "model"
	}
	return out
}

// shortNames maps generated component names such as
// application_models_property_UnitRole to their last segment (UnitRole),
// adding leading segments (property.UnitRole) until the display names are
// unique.
func shortNames(names []string) map[string]string {
	segments := map[string][]string{}
	take := map[string]int{}
	for _, name := range names {
		segments[name] = strings.Split(name, "_")
		take[name] = 1
	}

	display := func(name string) string {
		parts := segments[name]
		n := take[name]
		if n >= len(parts) {
			n = len(parts)
		}
		return strings.Join(parts[len(parts)-n:], ".")
	}

	for round := 0; round < 8; round++ {
		groups := map[string][]string{}
		for _, name := range names {
			groups[display(name)] = append(groups[display(name)], name)
		}
		changed := false
		for _, group := range groups {
			if len(group) < 2 {
				continue
			}
			for _, name := range group {
				if take[name] < len(segments[name]) {
					take[name]++
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}

	out := map[string]string{}
	for _, name := range names {
		out[name] = display(name)
		if out[name] == "" {
			out[name] = name
		}
	}
	return out
}

func (b *builder) buildSchemes() []*schemeView {
	global := map[string]bool{}
	for _, req := range b.d.Security {
		for name := range req {
			global[name] = true
		}
	}

	var out []*schemeView
	for _, name := range b.d.Components.SecuritySchemes.Keys {
		s := b.d.Components.SecuritySchemes.Map[name]
		if s == nil {
			continue
		}
		v := &schemeView{
			Name:        name,
			Anchor:      b.anchor("auth", slugify(name)),
			Kind:        s.Type,
			Description: markdown(s.Description),
			Global:      global[name],
		}
		b.schemes[name] = v.Anchor
		esc := template.HTMLEscapeString

		switch strings.ToLower(s.Type) {
		case "apikey":
			v.Kind = "API key"
			v.Detail = template.HTML("Send the key in the " + esc(s.In) + " <code>" + esc(s.Name) + "</code>.")
			v.Inputs = []authInput{{Key: name, Scheme: name, Label: s.Name, Kind: "apiKey", In: s.In, Param: s.Name, Placeholder: s.Name, Secret: true}}
		case "http":
			scheme := strings.ToLower(s.Scheme)
			switch scheme {
			case "bearer":
				v.Kind = "HTTP Bearer"
				detail := "Send <code>Authorization: Bearer &lt;token&gt;</code>."
				if s.BearerFormat != "" {
					detail += " Token format: " + esc(s.BearerFormat) + "."
				}
				v.Detail = template.HTML(detail)
				v.Inputs = []authInput{{Key: name, Scheme: name, Label: "Bearer token", Kind: "bearer", Placeholder: "token", Secret: true}}
			case "basic":
				v.Kind = "HTTP Basic"
				v.Detail = template.HTML("Send <code>Authorization: Basic &lt;base64(user:password)&gt;</code>.")
				v.Inputs = []authInput{
					{Key: name + ".user", Scheme: name, Label: "Username", Kind: "basic-user", Placeholder: "username"},
					{Key: name + ".pass", Scheme: name, Label: "Password", Kind: "basic-pass", Placeholder: "password", Secret: true},
				}
			default:
				v.Kind = "HTTP " + s.Scheme
				v.Detail = template.HTML("HTTP authentication scheme <code>" + esc(s.Scheme) + "</code>.")
			}
		case "oauth2":
			v.Kind = "OAuth 2.0"
			v.Detail = template.HTML("Send the access token as <code>Authorization: Bearer &lt;token&gt;</code>.")
			v.Inputs = []authInput{{Key: name, Scheme: name, Label: "Access token", Kind: "bearer", Placeholder: "token", Secret: true}}
		case "openidconnect":
			v.Kind = "OpenID Connect"
			v.Detail = template.HTML("Send the ID token as <code>Authorization: Bearer &lt;token&gt;</code>.")
			v.Inputs = []authInput{{Key: name, Scheme: name, Label: "Token", Kind: "bearer", Placeholder: "token", Secret: true}}
		case "mutualtls":
			v.Kind = "Mutual TLS"
			v.Detail = "Authenticate with a client certificate."
		}

		out = append(out, v)
	}
	return out
}

func (b *builder) requirements(reqs []map[string][]string) []requirementView {
	var out []requirementView
	for _, req := range reqs {
		names := make([]string, 0, len(req))
		for name := range req {
			names = append(names, name)
		}
		sort.Strings(names)

		var rv requirementView
		for _, name := range names {
			rv.Schemes = append(rv.Schemes, schemeRef{
				Name:   name,
				Anchor: b.schemes[name],
				Scopes: strings.Join(req[name], ", "),
			})
		}
		out = append(out, rv)
	}
	return out
}

func securityKey(reqs []map[string][]string) string {
	list := [][]string{}
	for _, req := range reqs {
		names := []string{}
		for name := range req {
			names = append(names, name)
		}
		sort.Strings(names)
		list = append(list, names)
	}
	data, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(data)
}

// buildGroups groups the operations the way their URLs nest: by first path
// segment, so /complexes/{complex_id}/invoices sits in complexes, with the
// groups sorted by name. Tags only describe: a group whose operations are
// exactly one tag's takes that tag's description, and an operation's tags
// stay searchable.
func (b *builder) buildGroups() []*groupView {
	byName := map[string]*groupView{}
	var groups []*groupView
	var ops []*opView
	// tag is the first tag shared by all of a group's operations, if any.
	tag := map[*groupView]string{}
	mixed := map[*groupView]bool{}
	tagOps := map[string]int{}
	for _, ref := range b.pageOperations() {
		ov := b.operationView(ref, ref.Operation.Tags)
		// Removed operations (diff mode) belong to the base document and
		// take no part in the head's production chains.
		if b.diff == nil || !b.diff.removed[ref.Operation] {
			ops = append(ops, ov)
		}

		name := pathGroup(ref.Path)
		g, ok := byName[name]
		if !ok {
			g = &groupView{Name: name}
			byName[name] = g
			groups = append(groups, g)
		}
		g.Ops = append(g.Ops, ov)

		first := ""
		if len(ref.Operation.Tags) > 0 {
			first = ref.Operation.Tags[0]
		}
		tagOps[first]++
		if len(g.Ops) == 1 {
			tag[g] = first
		} else if tag[g] != first {
			mixed[g] = true
		}
	}

	b.buildFlow(ops)

	descriptions := map[string]string{}
	for _, t := range b.d.Tags {
		_, exists := descriptions[t.Name]
		if !exists {
			descriptions[t.Name] = t.Description
		}
	}

	sort.SliceStable(groups, func(i, j int) bool {
		return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name)
	})
	for _, g := range groups {
		g.Anchor = b.anchor("path", slugify(g.Name))
		g.NavTree = navTree(g.Ops)
		name := tag[g]
		if name != "" && !mixed[g] && tagOps[name] == len(g.Ops) {
			g.Description = markdown(descriptions[name])
		}
	}
	return groups
}

// pathGroup is the group of a path: its first segment, or / for the root.
func pathGroup(path string) string {
	segs := pathSegments(path)
	if len(segs) == 0 {
		return "/"
	}
	return segs[0]
}

// tagNames lists the operation tags in the order the document declares them,
// then undeclared ones by name, and Other last for untagged operations.
func (d *Document) tagNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range d.Tags {
		if !seen[t.Name] {
			seen[t.Name] = true
			out = append(out, t.Name)
		}
	}
	var extra []string
	other := false
	for _, ref := range d.Operations() {
		if len(ref.Operation.Tags) == 0 {
			other = true
		}
		for _, name := range ref.Operation.Tags {
			if !seen[name] {
				seen[name] = true
				extra = append(extra, name)
			}
		}
	}
	sort.SliceStable(extra, func(i, j int) bool {
		return strings.ToLower(extra[i]) < strings.ToLower(extra[j])
	})
	out = append(out, extra...)
	if other && !seen["Other"] {
		out = append(out, "Other")
	}
	return out
}

// navTree groups a group's operations into folders by path, the way a file
// browser would. The path prefix every operation shares is dropped, so the
// first level is the resource. Parameter segments such as {id} never make a
// folder; their operations belong to the static segment above them. A static
// segment becomes a folder only when at least two operations sit beneath it;
// otherwise its operation joins the enclosing folder. On top of that:
//
//   - a top-level folder that only scopes its children by id, like
//     complexes in /complexes/{complex_id}/invoices when a few operations of
//     the group live elsewhere, is dissolved into the top level;
//   - a folder whose only content is one sub-folder is shown as one compact
//     folder, finance/utilities;
//   - groups of up to navFlatMax operations, and groups whose operations all end
//     up on one level, stay a flat list.
func navTree(ops []*opView) *navFolder {
	if len(ops) <= navFlatMax {
		return &navFolder{Count: len(ops), Ops: ops}
	}

	order := map[*opView]int{}
	paths := make([][]string, len(ops))
	for i, ov := range ops {
		order[ov] = i
		paths[i] = pathSegments(ov.Path)
	}
	prefix := commonPrefix(paths)

	root := newNavNode()
	for i, segs := range paths {
		n := root
		for j := prefix; j < len(segs); j++ {
			if isParamSegment(segs[j]) {
				continue
			}
			child, ok := n.children[segs[j]]
			if !ok {
				child = newNavNode()
				n.children[segs[j]] = child
			}
			if j+1 >= len(segs) || !isParamSegment(segs[j+1]) {
				child.scope = false
			}
			n = child
		}
		n.ops = append(n.ops, ops[i])
	}

	out := root.fold(order)
	out.Folders = hoistScopes(out.Folders)
	out.compact()
	// A lone folder at the top says nothing the group name does not.
	for len(out.Ops) == 0 && len(out.Folders) == 1 {
		out = &navFolder{Count: out.Count, Ops: out.Folders[0].Ops, Folders: out.Folders[0].Folders}
	}
	return out
}

type navNode struct {
	ops      []*opView
	children map[string]*navNode
	scope    bool // every path through the segment continues with a parameter
}

func newNavNode() *navNode {
	return &navNode{children: map[string]*navNode{}, scope: true}
}

func (n *navNode) fold(order map[*opView]int) *navFolder {
	f := &navFolder{Ops: append([]*opView(nil), n.ops...), scope: n.scope}

	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		child := n.children[name].fold(order)
		if child.Count >= 2 || len(child.Folders) > 0 {
			child.Name = name
			f.Folders = append(f.Folders, child)
			f.Count += child.Count
			continue
		}
		f.Ops = append(f.Ops, child.Ops...)
	}

	sort.SliceStable(f.Ops, func(i, j int) bool {
		return order[f.Ops[i]] < order[f.Ops[j]]
	})
	f.Count += len(f.Ops)
	return f
}

// hoistScopes replaces top-level folders that only scope their sub-folders
// by an id with those sub-folders, unless a name would then appear twice.
func hoistScopes(folders []*navFolder) []*navFolder {
	names := map[string]int{}
	for _, f := range folders {
		names[f.Name]++
	}
	var out []*navFolder
	hoisted := false
	for _, f := range folders {
		if !f.scope || len(f.Ops) > 0 {
			out = append(out, f)
			continue
		}
		clash := false
		for _, sub := range f.Folders {
			if names[sub.Name] > 0 {
				clash = true
			}
		}
		if clash {
			out = append(out, f)
			continue
		}
		for _, sub := range f.Folders {
			names[sub.Name]++
		}
		out = append(out, f.Folders...)
		hoisted = true
	}
	if hoisted {
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].Name < out[j].Name
		})
	}
	return out
}

// compact merges every folder that holds nothing but one sub-folder into a
// single a/b folder.
func (f *navFolder) compact() {
	for _, sub := range f.Folders {
		for len(sub.Ops) == 0 && len(sub.Folders) == 1 {
			inner := sub.Folders[0]
			sub.Name += "/" + inner.Name
			sub.Ops = inner.Ops
			sub.Folders = inner.Folders
		}
		sub.compact()
	}
}

func pathSegments(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func isParamSegment(seg string) bool {
	return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")
}

// commonPrefix returns how many leading segments every path shares, treating
// all parameter segments as equal.
func commonPrefix(paths [][]string) int {
	if len(paths) == 0 {
		return 0
	}
	n := len(paths[0])
	for _, p := range paths[1:] {
		if len(p) < n {
			n = len(p)
		}
		for i := 0; i < n; i++ {
			same := p[i] == paths[0][i] || (isParamSegment(p[i]) && isParamSegment(paths[0][i]))
			if !same {
				n = i
				break
			}
		}
	}
	return n
}

// crumbView is one segment of an operation's path bar. Depth is how many
// segments the prefix up to and including it holds.
type crumbView struct {
	Name  string
	Param bool
	Depth int
}

func crumbs(path string) []crumbView {
	var out []crumbView
	for i, seg := range pathSegments(path) {
		out = append(out, crumbView{Name: seg, Param: isParamSegment(seg), Depth: i + 1})
	}
	return out
}

func (b *builder) buildOperation(ref OperationRef, tags []string) *opView {
	op := ref.Operation
	slug := op.OperationID
	if slug == "" {
		slug = ref.Method + "-" + ref.Path
	}

	ov := &opView{
		Anchor:      b.anchor("operation", slugify(slug)),
		Method:      ref.Method,
		MethodClass: strings.ToLower(ref.Method),
		Path:        ref.Path,
		Crumbs:      crumbs(ref.Path),
		Title:       op.Summary,
		OperationID: op.OperationID,
		Deprecated:  op.Deprecated,
		Internal:    op.Internal,
		Description: markdown(op.Description),
		ref:         ref,
	}
	if ov.Title == "" {
		ov.Title = op.OperationID
	}
	if ov.Title == "" {
		ov.Title = ref.Method + " " + ref.Path
	}
	if ov.Description == "" && ref.Item.Description != "" {
		ov.Description = markdown(ref.Item.Description)
	}
	ov.Search = strings.ToLower(strings.Join(append([]string{ov.Title, ref.Method, ref.Path, op.OperationID}, tags...), " "))

	reqs := b.d.OperationSecurity(op)
	ov.Security = b.requirements(reqs)
	ov.NoAuth = len(reqs) == 0
	ov.SecurityKey = securityKey(reqs)

	b.buildParameters(ov, ref)
	b.buildBody(ov, op)
	b.buildResponses(ov, op)

	// Without an explicit ServerURL, try-it and the copied curl use the
	// operation's server before servers[0], as Curl does.
	ov.CurlAuth = b.d.curlAuth(op)
	if b.opts.ServerURL == "" && len(op.Servers) > 0 {
		ov.TryServer = op.Servers[0].Expand()
	}

	return ov
}

func (b *builder) buildParameters(ov *opView, ref OperationRef) {
	params := b.d.Parameters(ref.Item, ref.Operation)

	var current *paramGroup
	var rows, optional strings.Builder
	flush := func() {
		if current != nil {
			current.Rows = template.HTML(rows.String())
			current.Optional = template.HTML(optional.String())
			ov.ParamGroups = append(ov.ParamGroups, *current)
			rows.Reset()
			optional.Reset()
		}
	}

	for _, p := range params {
		if current == nil || current.In != p.In {
			flush()
			current = &paramGroup{In: p.In}
		}

		schema := parameterSchema(p)
		t := b.newTree(ForRequest, false)
		t.paramRow(p.Name, schema, p.Description, p.Required, p.Deprecated, paramExample(b.d, p, false))
		if p.Required {
			rows.WriteString(t.String())
		} else {
			optional.WriteString(t.String())
		}

		if p.In == "cookie" {
			continue
		}
		tp := tryParam{Name: p.Name, In: p.In, Required: p.Required}
		f := b.d.Flatten(schema)
		tp.Array = f != nil && isArray(f)
		if tp.Array {
			// Probe ParamValues so try-it serializes arrays the way curl does:
			// repeated keys, or one value joined with the style's separator.
			probe := ParamValues(p, []any{"a", "b"})
			tp.Explode = len(probe) > 1
			if len(probe) == 1 {
				tp.Sep = strings.TrimSuffix(strings.TrimPrefix(probe[0], "a"), "b")
			}
		}
		tp.Placeholder = ParamString(paramExample(b.d, p, true))
		if tp.Placeholder == "" {
			tp.Placeholder = b.newTree(ForRequest, false).plainLabel(schema)
		}
		ov.TryParams = append(ov.TryParams, tp)
	}
	flush()
}

// parameterSchema returns the schema of p, falling back to its first content
// type for content-encoded parameters.
func parameterSchema(p *Parameter) *Schema {
	if p.Schema != nil {
		return p.Schema
	}
	for _, key := range p.Content.Keys {
		mt := p.Content.Map[key]
		if mt != nil && mt.Schema != nil {
			return mt.Schema
		}
	}
	return nil
}

// paramExample is the documented example of p (example, then the first
// named example) or, when generate is set, the schema's example value.
func paramExample(d *Document, p *Parameter, generate bool) any {
	if p.Example != nil {
		return p.Example
	}
	for _, key := range p.Examples.Keys {
		e := d.Example(p.Examples.Map[key])
		if e != nil && e.Value != nil {
			return e.Value
		}
	}
	schema := parameterSchema(p)
	if !generate || schema == nil {
		return nil
	}
	return d.ExampleValue(schema, ForRequest)
}

func (b *builder) buildBody(ov *opView, op *Operation) {
	body := b.d.RequestBody(op.RequestBody)
	if body == nil || body.Content.Len() == 0 && body.Description == "" {
		return
	}

	bv := &bodyView{Required: body.Required, Description: markdown(body.Description)}
	for _, ct := range body.Content.Keys {
		mt := body.Content.Map[ct]
		if mt == nil {
			continue
		}
		if len(bv.Contents) == 0 {
			bv.Summary = b.summary(mt.Schema, ForRequest)
		}
		bv.Contents = append(bv.Contents, contentView{Type: ct, Tree: b.newTree(ForRequest, true).root(mt.Schema, "")})

		if !ov.TryBody {
			ov.TryBody = true
			ov.TryCT = ct
			ov.ReqExample = textHTML(b.d.MediaExampleJSON(mt, ForRequest))
			ov.ReqTypes = b.typesRef(mt, ForRequest)
		}
	}
	ov.Body = bv
}

func statusRank(code string) int {
	if code == "default" {
		return 10000
	}
	n, err := strconv.Atoi(code)
	if err == nil {
		return n * 10
	}
	if len(code) == 3 && (code[1] == 'X' || code[1] == 'x') && code[0] >= '1' && code[0] <= '5' {
		return int(code[0]-'0')*1000 + 999
	}
	return 9000
}

func statusClass(code string) string {
	if code == "" || code == "default" {
		return "sdefault"
	}
	switch code[0] {
	case '1':
		return "s1xx"
	case '2':
		return "s2xx"
	case '3':
		return "s3xx"
	case '4':
		return "s4xx"
	case '5':
		return "s5xx"
	}
	return "sdefault"
}

// buildResponses adds a sample per documented response, in status order,
// for the Response card the machine's output ports switch.
func (b *builder) buildResponses(ov *opView, op *Operation) {
	codes := append([]string(nil), op.Responses.Keys...)
	sort.SliceStable(codes, func(i, j int) bool {
		return statusRank(codes[i]) < statusRank(codes[j])
	})

	for _, code := range codes {
		r := b.d.Response(op.Responses.Map[code])
		if r == nil {
			continue
		}
		for _, ct := range r.Content.Keys {
			mt := r.Content.Map[ct]
			if mt == nil {
				continue
			}
			example := b.d.MediaExampleJSON(mt, ForResponse)
			if example != "" {
				ov.ResSamples = append(ov.ResSamples, sampleView{Label: code, Class: statusClass(code), Type: ct, Model: b.sampleModel(mt.Schema), Example: textHTML(example), Types: b.typesRef(mt, ForResponse)})
				break
			}
		}
	}
}

// sampleModel names the component schema s stands for, or the one its
// items do, or returns nil for an inline schema.
func (b *builder) sampleModel(s *Schema) *sampleModelView {
	name := b.d.SchemaName(s)
	suffix := ""
	if name == "" {
		f := b.d.Flatten(s)
		if f == nil || !isArray(f) || f.Items == nil {
			return nil
		}
		name = b.d.SchemaName(f.Items)
		suffix = "[]"
	}
	anchor, ok := b.models[name]
	if !ok {
		return nil
	}
	class := "t-prim"
	f := b.d.Flatten(&Schema{Ref: refSchemas + name})
	switch {
	case f != nil && len(f.Enum) > 0:
		class = "t-enum"
	case f != nil && (isObject(f) || isArray(f)):
		class = "t-model"
	}
	label := b.display(name)
	cut := strings.LastIndex(label, ".") + 1
	return &sampleModelView{Anchor: anchor, Prefix: label[:cut], Name: label[cut:] + suffix, Title: name, Class: class, Diff: b.modelBadge(name)}
}

// summary names what s carries: its model, a list of models, or its label.
func (b *builder) summary(s *Schema, mode ExampleMode) summaryView {
	f := b.d.Flatten(s)
	if f == nil {
		return summaryView{}
	}
	name := b.d.SchemaName(s)
	switch {
	case name != "":
		return summaryView{Type: template.HTML(b.typeLink(name))}
	case isArray(f) && f.Items != nil && b.d.SchemaName(f.Items) != "":
		return summaryView{Type: template.HTML(b.typeLink(b.d.SchemaName(f.Items)) + `<span class="arr">[]</span>`)}
	}
	return summaryView{Type: template.HTML(`<span class="ty">` + esc(b.newTree(mode, true).plainLabel(s)) + `</span>`)}
}

// typesRef describes the types of a media type's example values once per
// page and returns the description's index in the page's type notes, or ""
// when there is nothing to describe. Many operations share a response model,
// so most samples point at a description already listed.
func (b *builder) typesRef(mt *MediaType, mode ExampleMode) string {
	notes := b.d.mediaTypes(mt, mode, b.display)
	if notes == "" {
		return ""
	}
	return strconv.Itoa(b.noteRef(notes))
}

// noteRef lists a type description once per page and returns its index.
func (b *builder) noteRef(notes string) int {
	i, ok := b.noteIndex[notes]
	if !ok {
		i = len(b.notes)
		b.notes = append(b.notes, notes)
		b.noteIndex[notes] = i
	}
	return i
}

// modelSample is a model's example for the model dialog, which shows it as
// the ObjectInspector: Value is the example as JSON, Types the index of
// its type notes.
type modelSample struct {
	Value json.RawMessage `json:"v"`
	Types int             `json:"t"`
}

// addModelSample records the dialog sample of the component schema name.
func (b *builder) addModelSample(name string) {
	v, notes := b.d.modelExample(&Schema{Ref: refSchemas + name}, b.display)
	if len(notes) == 0 {
		return
	}
	var raw bytes.Buffer
	appendJSON(&raw, v)
	b.samples[b.models[name]] = modelSample{Value: raw.Bytes(), Types: b.noteRef(strings.Join(notes, "\n"))}
}

// modelSamplesJSON is the dialog samples by model anchor. Marshal escapes
// <, > and & in the examples, so the JSON is safe inside a script element.
func (b *builder) modelSamplesJSON() template.JS {
	out, err := json.Marshal(b.samples)
	if err != nil {
		return "{}"
	}
	return template.JS(out)
}

func (b *builder) typeNotesJSON() template.JS {
	out, err := json.Marshal(b.notes)
	if err != nil || b.notes == nil {
		return "[]"
	}
	return template.JS(out)
}

// display is the display name of a component schema.
func (b *builder) display(name string) string {
	short, ok := b.short[name]
	if !ok {
		return name
	}
	return short
}

// typeLink links the model name, with its package prefix muted.
func (b *builder) typeLink(name string) string {
	short, ok := b.short[name]
	if !ok {
		return `<span class="ref">` + esc(name) + `</span>`
	}
	cut := strings.LastIndex(short, ".") + 1
	pre := ""
	if cut > 0 {
		pre = `<span class="pre">` + esc(short[:cut]) + `</span>`
	}
	return `<a class="ref" href="#` + esc(b.models[name]) + `" title="` + esc(name) + `">` + pre + esc(short[cut:]) + `</a>`
}

// summaryFieldsMax caps the field names listed after a collapsed row.
const summaryFieldsMax = 6

// fieldNames lists up to summaryFieldsMax property names of the flattened
// object f that apply in mode, and how many more there are.
func fieldNames(d *Document, f *Schema, mode ExampleMode) ([]string, int) {
	var names []string
	more := 0
	for _, key := range f.Properties.Keys {
		p := d.Flatten(f.Properties.Map[key])
		if p != nil && (mode == ForRequest && p.ReadOnly || mode == ForResponse && p.WriteOnly) {
			continue
		}
		if len(names) == summaryFieldsMax {
			more++
			continue
		}
		names = append(names, key)
	}
	return names, more
}

func (b *builder) buildModels() []*modelView {
	var out []*modelView
	for _, name := range b.d.Components.Schemas.Keys {
		s := b.d.Components.Schemas.Map[name]
		if s == nil {
			continue
		}
		short := b.short[name]
		table := b.d.Table(name)
		b.addModelSample(name)
		mv := &modelView{
			Name:        name,
			Short:       short,
			Anchor:      b.models[name],
			Table:       table,
			Description: markdown(s.Description),
			Tree:        b.newTree(ForResponse, false).root(s, name),
			Search:      strings.ToLower(strings.TrimSpace(short + " " + name + " " + table)),
			MadeBy:      len(b.flow.producers[name]),
			UsedBy:      len(b.flow.consumers[name]),
			Trace:       b.flow.ids[name],
			Diff:        b.modelBadge(name),
		}
		_, mv.Inspector = b.samples[mv.Anchor]
		out = append(out, mv)
	}
	return append(out, b.removedModelViews()...)
}

func (b *builder) buildModelNav(models []*modelView) modelNav {
	var nav modelNav
	for _, m := range models {
		if m.Table != "" {
			nav.Tables++
		}
	}
	tagged := b.d.hasModelTags() || b.diff != nil && b.diff.base.hasModelTags()
	if tagged {
		nav.Groups = b.tagGroups(models)
		return nav
	}

	byShort := func(list []*modelView) {
		sort.SliceStable(list, func(i, j int) bool {
			return strings.ToLower(list[i].Short) < strings.ToLower(list[j].Short)
		})
	}
	byModule := map[string]int{}
	for _, m := range models {
		module := schemaGroup(m.Name)
		if module == "" {
			nav.Loose = append(nav.Loose, m)
			continue
		}
		i, ok := byModule[module]
		if !ok {
			i = len(nav.Groups)
			byModule[module] = i
			nav.Groups = append(nav.Groups, modelGroup{Name: module})
		}
		nav.Groups[i].Models = append(nav.Groups[i].Models, m)
	}
	byShort(nav.Loose)
	for i := range nav.Groups {
		byShort(nav.Groups[i].Models)
		nav.Groups[i].Changes = groupChanges(nav.Groups[i].Models)
	}
	sort.SliceStable(nav.Groups, func(i, j int) bool {
		return nav.Groups[i].Name < nav.Groups[j].Name
	})
	return nav
}
