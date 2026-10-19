package apidocs

import (
	"encoding/json"
	"fmt"
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
	Tags        []*tagView
	Models      []*modelView
	AssetsURL   string
	CSS         template.CSS
	JS          template.JS
}

type serverView struct {
	URL         string
	Description string
	Selected    bool
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

type tagView struct {
	Name        string
	Anchor      string
	Description template.HTML
	Ops         []*opView // operations rendered under this tag
	Nav         []*opView // sidebar entries, including operations rendered under another tag
}

type opView struct {
	Anchor      string
	Method      string
	MethodClass string
	Path        string
	PathHTML    template.HTML
	Title       string
	OperationID string
	Deprecated  bool
	Description template.HTML
	Security    []requirementView
	NoAuth      bool
	SecurityKey string // JSON list of requirement scheme lists, read by the try-it client
	ParamGroups []paramGroup
	Body        *bodyView
	Responses   []*responseView
	Curl        template.HTML
	ReqSamples  []sampleView
	ResSamples  []sampleView
	Search      string
	TryParams   []tryParam
	TryBody     bool
	TryCT       string
}

type paramGroup struct {
	Title string
	Rows  template.HTML
}

type bodyView struct {
	Required    bool
	Description template.HTML
	Contents    []contentView
}

type contentView struct {
	Type string
	Tree template.HTML
}

type responseView struct {
	HasBody     bool
	Open        bool
	Status      string
	Class       string
	Description template.HTML
	Headers     template.HTML
	Contents    []contentView
}

type sampleView struct {
	Label   string
	Class   string
	Type    string
	Example template.HTML
}

// textEscaper escapes text for element content only. html/template's escaper
// also turns every quote into &#34;, which grows JSON samples by a third.
var textEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func textHTML(s string) template.HTML {
	return template.HTML(textEscaper.Replace(s))
}

type tryParam struct {
	Name        string
	In          string
	Required    bool
	Value       string
	Placeholder string
}

type modelView struct {
	Name   string
	Short  string
	Anchor string
	Tree   template.HTML
	Search string
}

type builder struct {
	d       *Document
	opts    Options
	used    map[string]bool
	short   map[string]string // component schema name to display name
	models  map[string]string // component schema name to anchor
	schemes map[string]string // security scheme name to anchor
	server  string
}

func buildPage(d *Document, opts Options) *pageView {
	b := &builder{
		d:       d,
		opts:    opts,
		used:    map[string]bool{},
		models:  map[string]string{},
		schemes: map[string]string{},
	}

	b.short = shortNames(d.Components.Schemas.Keys)
	for _, name := range d.Components.Schemas.Keys {
		b.models[name] = b.anchor("model", modelSlug(name))
	}

	b.server = opts.ServerURL
	if b.server == "" && len(d.Servers) > 0 {
		b.server = d.Servers[0].URL
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
		p.Servers = append(p.Servers, serverView{URL: s.URL, Description: s.Description, Selected: s.URL == b.server})
	}
	if opts.ServerURL != "" && !hasServer(d.Servers, opts.ServerURL) {
		p.Servers = append([]serverView{{URL: opts.ServerURL, Selected: true}}, p.Servers...)
	}

	p.Schemes = b.buildSchemes()
	p.Global = b.requirements(d.Security)
	p.Tags = b.buildTags()
	p.Models = b.buildModels()

	return p
}

func hasServer(servers []Server, url string) bool {
	for _, s := range servers {
		if s.URL == url {
			return true
		}
	}
	return false
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

func (b *builder) buildTags() []*tagView {
	byName := map[string]*tagView{}
	var declared []*tagView
	for _, t := range b.d.Tags {
		_, exists := byName[t.Name]
		if exists {
			continue
		}
		tv := &tagView{Name: t.Name, Description: markdown(t.Description)}
		byName[t.Name] = tv
		declared = append(declared, tv)
	}

	var extra []*tagView
	tagFor := func(name string) *tagView {
		tv, ok := byName[name]
		if ok {
			return tv
		}
		tv = &tagView{Name: name}
		byName[name] = tv
		extra = append(extra, tv)
		return tv
	}

	for _, ref := range b.d.Operations() {
		tags := ref.Operation.Tags
		if len(tags) == 0 {
			tags = []string{"Other"}
		}
		ov := b.buildOperation(ref, tags)
		for i, name := range tags {
			tv := tagFor(name)
			tv.Nav = append(tv.Nav, ov)
			if i == 0 {
				tv.Ops = append(tv.Ops, ov)
			}
		}
	}

	sort.SliceStable(extra, func(i, j int) bool {
		if extra[i].Name == "Other" || extra[j].Name == "Other" {
			return extra[j].Name == "Other" && extra[i].Name != "Other"
		}
		return strings.ToLower(extra[i].Name) < strings.ToLower(extra[j].Name)
	})

	var out []*tagView
	for _, tv := range append(declared, extra...) {
		if len(tv.Nav) == 0 {
			continue
		}
		tv.Anchor = b.anchor("tag", slugify(tv.Name))
		out = append(out, tv)
	}
	return out
}

var pathParam = regexp.MustCompile(`\{[^}/]+\}`)

func pathHTML(path string) template.HTML {
	var sb strings.Builder
	last := 0
	for _, loc := range pathParam.FindAllStringIndex(path, -1) {
		sb.WriteString(template.HTMLEscapeString(path[last:loc[0]]))
		sb.WriteString(`<span class="pp">`)
		sb.WriteString(template.HTMLEscapeString(path[loc[0]:loc[1]]))
		sb.WriteString(`</span>`)
		last = loc[1]
	}
	sb.WriteString(template.HTMLEscapeString(path[last:]))
	return template.HTML(sb.String())
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
		PathHTML:    pathHTML(ref.Path),
		Title:       op.Summary,
		OperationID: op.OperationID,
		Deprecated:  op.Deprecated,
		Description: markdown(op.Description),
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

	ov.Curl = textHTML(b.d.Curl(ref.Method, ref.Path, ref.Item, op, b.server))

	return ov
}

var locationTitles = map[string]string{
	"path":   "Path parameters",
	"query":  "Query parameters",
	"header": "Header parameters",
	"cookie": "Cookie parameters",
}

func (b *builder) buildParameters(ov *opView, ref OperationRef) {
	params := b.d.Parameters(ref.Item, ref.Operation)

	var current *paramGroup
	var rows strings.Builder
	flush := func() {
		if current != nil {
			current.Rows = template.HTML(rows.String())
			ov.ParamGroups = append(ov.ParamGroups, *current)
			rows.Reset()
		}
	}

	for _, p := range params {
		title, ok := locationTitles[p.In]
		if !ok {
			title = p.In + " parameters"
		}
		if current == nil || current.Title != title {
			flush()
			current = &paramGroup{Title: title}
		}

		schema := parameterSchema(p)
		t := b.newTree(ForRequest, false)
		t.paramRow(p.Name, schema, p.Description, p.Required, p.Deprecated, documentedExample(b.d, p))
		rows.WriteString(t.String())

		if p.In == "cookie" {
			continue
		}
		tp := tryParam{Name: p.Name, In: p.In, Required: p.Required}
		example := scalarString(tryExample(b.d, p, schema))
		if p.Required || p.In == "path" {
			tp.Value = example
		}
		tp.Placeholder = example
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

// documentedExample is the documented example of p, if any.
func documentedExample(d *Document, p *Parameter) any {
	if p.Example != nil {
		return p.Example
	}
	for _, key := range p.Examples.Keys {
		e := d.Example(p.Examples.Map[key])
		if e != nil && e.Value != nil {
			return e.Value
		}
	}
	return nil
}

// tryExample prefers documented values, then the generated example.
func tryExample(d *Document, p *Parameter, schema *Schema) any {
	v := documentedExample(d, p)
	if v != nil {
		return v
	}
	if schema == nil {
		return nil
	}
	f := d.Flatten(schema)
	if f == nil {
		return nil
	}
	if f.Example != nil {
		return f.Example
	}
	if f.Default != nil {
		return f.Default
	}
	if len(f.Enum) > 0 {
		return f.Enum[0]
	}
	return d.ExampleValue(schema, ForRequest)
}

// scalarString renders a parameter value the way it travels in a URL or
// header: strings verbatim, arrays comma-joined, everything else as JSON.
func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			parts = append(parts, scalarString(item))
		}
		return strings.Join(parts, ",")
	default:
		data, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(data)
	}
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
		bv.Contents = append(bv.Contents, contentView{Type: ct, Tree: b.newTree(ForRequest, true).root(mt.Schema, "")})

		example := b.d.MediaExampleJSON(mt, ForRequest)
		if example != "" {
			ov.ReqSamples = append(ov.ReqSamples, sampleView{Label: ct, Type: ct, Example: textHTML(example)})
		}
		if !ov.TryBody {
			ov.TryBody = true
			ov.TryCT = ct
		}
	}
	if len(ov.ReqSamples) == 1 {
		ov.ReqSamples[0].Label = "Body"
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

func (b *builder) buildResponses(ov *opView, op *Operation) {
	codes := append([]string(nil), op.Responses.Keys...)
	sort.SliceStable(codes, func(i, j int) bool {
		return statusRank(codes[i]) < statusRank(codes[j])
	})

	opened := false
	for _, code := range codes {
		r := b.d.Response(op.Responses.Map[code])
		if r == nil {
			continue
		}
		rv := &responseView{Status: code, Class: statusClass(code), Description: markdown(r.Description)}

		if r.Headers.Len() > 0 {
			t := b.newTree(ForResponse, false)
			for _, name := range r.Headers.Keys {
				h := b.d.Header(r.Headers.Map[name])
				if h == nil {
					continue
				}
				t.paramRow(name, parameterSchema(h), h.Description, h.Required, h.Deprecated, documentedExample(b.d, h))
			}
			rv.Headers = template.HTML(t.String())
		}

		sampled := false
		for _, ct := range r.Content.Keys {
			mt := r.Content.Map[ct]
			if mt == nil {
				continue
			}
			rv.Contents = append(rv.Contents, contentView{Type: ct, Tree: b.newTree(ForResponse, true).root(mt.Schema, "")})
			if sampled {
				continue
			}
			example := b.d.MediaExampleJSON(mt, ForResponse)
			if example != "" {
				ov.ResSamples = append(ov.ResSamples, sampleView{Label: code, Class: rv.Class, Type: ct, Example: textHTML(example)})
				sampled = true
			}
		}

		rv.HasBody = len(rv.Contents) > 0 || rv.Headers != ""
		if rv.HasBody && !opened {
			rv.Open = true
			opened = true
		}
		ov.Responses = append(ov.Responses, rv)
	}
}

func (b *builder) buildModels() []*modelView {
	var out []*modelView
	for _, name := range b.d.Components.Schemas.Keys {
		s := b.d.Components.Schemas.Map[name]
		if s == nil {
			continue
		}
		short := b.short[name]
		out = append(out, &modelView{
			Name:   name,
			Short:  short,
			Anchor: b.models[name],
			Tree:   b.newTree(ForResponse, false).root(s, name),
			Search: strings.ToLower(short + " " + name),
		})
	}
	return out
}
