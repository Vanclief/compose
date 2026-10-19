package reference

import (
	"encoding/json"
	"html/template"
	"sort"
	"strconv"
	"strings"
)

// The flow view reads each operation as a machine: inputs on the left, the
// responses it can produce on the right. Operations are also linked into
// production chains. An operation produces a model when a success response
// returns it, directly or as a list. Another operation consumes that model
// when one of its inputs is the model's id: a path parameter {id} under the
// model's collection, or a parameter or body field named <model>_id.

// machineView is the input and output ports of one operation.
type machineView struct {
	Resource string
	Inputs   []portView
	Outputs  []portView
}

// portView is one input or output of a machine.
type portView struct {
	Key         string // data-port value: in:<location>:<name>, in:optional, in:body or out:<status>
	Name        string
	Kind        string // type shape: str, num, bool, obj, arr or any
	Type        string // short type label
	Title       string // full type label, for the tooltip
	Required    bool
	Stack       bool   // the port carries a list
	Model       string // trace ids of the models the port carries, space separated
	Source      *opView
	MoreSources int
	Users       int // operations that consume what an output produces
	Class       string
	Main        bool // success output
	Ghost       bool // placeholder for an operation without inputs
	Fields      []portView
	MoreFields  int
	Names       []string // optional parameter names
}

// BaseType is Type without its list brackets: Amenity for Amenity[].
func (p portView) BaseType() string {
	return strings.TrimRight(p.Type, "[]")
}

// ListSuffix is the list brackets Type ends with, if any.
func (p portView) ListSuffix() string {
	return p.Type[len(p.BaseType()):]
}

// flowFieldsMax caps the id fields listed under a body port.
const flowFieldsMax = 4

type flowIndex struct {
	producers map[string][]*opView // model name to producing operations
	consumers map[string][]*opView // model name to consuming operations
	byKey     map[string]string    // normalized model name to model name
	ids       map[string]string    // model name to the short id that traces it in the page
	JSON      template.JS
}

// opFlow is what the flow pass keeps per operation while it links them.
type opFlow struct {
	produced []string // models produced by success responses
	list     bool     // a success response returns a list
	consumed []string
}

// buildFlow computes the machine of every operation and the production
// chains between them.
func (b *builder) buildFlow(ops []*opView) {
	idx := &flowIndex{producers: map[string][]*opView{}, consumers: map[string][]*opView{}, byKey: map[string]string{}, ids: map[string]string{}}
	b.flow = idx
	for i, name := range b.d.Components.Schemas.Keys {
		idx.ids[name] = "m" + strconv.Itoa(i)
	}

	flows := map[*opView]*opFlow{}
	for _, ov := range ops {
		of := &opFlow{}
		flows[ov] = of
		for _, code := range ov.ref.Operation.Responses.Keys {
			if !strings.HasPrefix(code, "2") {
				continue
			}
			s := b.responseSchema(ov.ref.Operation.Responses.Map[code])
			names, list := b.produced(s)
			of.list = of.list || list
			for _, name := range names {
				if !contains(of.produced, name) {
					of.produced = append(of.produced, name)
					idx.producers[name] = append(idx.producers[name], ov)
				}
			}
		}
	}

	// Inputs find their model by its display name; when two models share a
	// name, the one more operations produce wins, then document order.
	for _, name := range b.d.Components.Schemas.Keys {
		key := normKey(lastSegment(b.short[name]))
		current, ok := idx.byKey[key]
		if !ok || len(idx.producers[name]) > len(idx.producers[current]) {
			idx.byKey[key] = name
		}
	}
	// A list whose items are a variant of the model, such as complexes
	// returning ComplexWithRole, still produces the model its collection
	// is named after.
	for _, ov := range ops {
		of := flows[ov]
		segs := pathSegments(ov.Path)
		if ov.Method != "GET" || !of.list || len(segs) == 0 || isParamSegment(segs[len(segs)-1]) {
			continue
		}
		model := b.resolve("id", segs[len(segs)-1])
		if model != "" && !contains(of.produced, model) {
			of.produced = append(of.produced, model)
			idx.producers[model] = append(idx.producers[model], ov)
		}
	}
	for _, producers := range idx.producers {
		sort.SliceStable(producers, func(i, j int) bool {
			return producerRank(producers[i], flows[producers[i]]) < producerRank(producers[j], flows[producers[j]])
		})
	}

	for _, ov := range ops {
		b.buildMachine(ov, flows[ov])
	}

	for _, ov := range ops {
		of := flows[ov]
		for i := range ov.Machine.Outputs {
			p := &ov.Machine.Outputs[i]
			if !p.Main {
				continue
			}
			users := map[*opView]bool{}
			for _, name := range of.produced {
				for _, c := range idx.consumers[name] {
					if c != ov {
						users[c] = true
					}
				}
			}
			p.Users = len(users)
		}
		var trace []string
		for _, name := range append(append([]string(nil), of.produced...), of.consumed...) {
			id := b.flow.ids[name]
			if id != "" && !contains(trace, id) {
				trace = append(trace, id)
			}
		}
		ov.Trace = strings.Join(trace, " ")
	}

	idx.JSON = b.flowJSON(ops, flows)
}

// producerRank orders the operations that produce a model: lists first, then
// creates, reads and the rest.
func producerRank(ov *opView, of *opFlow) int {
	switch {
	case ov.Method == "GET" && of.list:
		return 0
	case ov.Method == "POST":
		return 1
	case ov.Method == "GET":
		return 2
	}
	return 3
}

// responseSchema is the schema of a response's first content type.
func (b *builder) responseSchema(r *Response) *Schema {
	r = b.d.Response(r)
	if r == nil {
		return nil
	}
	for _, ct := range r.Content.Keys {
		mt := r.Content.Map[ct]
		if mt != nil && mt.Schema != nil {
			return mt.Schema
		}
	}
	return nil
}

// produced names the models a response schema returns, and whether it
// returns them as a list: an array of a model, or a wrapper object without
// an id of its own whose array fields hold models.
func (b *builder) produced(s *Schema) ([]string, bool) {
	f := b.d.Flatten(s)
	if f == nil {
		return nil, false
	}
	if isArray(f) {
		name := b.d.SchemaName(f.Items)
		if name == "" {
			return nil, false
		}
		return []string{name}, true
	}
	name := b.d.SchemaName(s)
	if !isObject(f) {
		return nil, false
	}
	_, hasID := f.Properties.Get("id")
	if !hasID {
		var names []string
		for _, key := range f.Properties.Keys {
			p := b.d.Flatten(f.Properties.Map[key])
			if p == nil || !isArray(p) {
				continue
			}
			item := b.d.SchemaName(p.Items)
			if item != "" && !contains(names, item) {
				names = append(names, item)
			}
		}
		if len(names) > 0 {
			return names, true
		}
	}
	if name == "" {
		return nil, false
	}
	return []string{name}, false
}

// resolve returns the model an input named name carries: the base of
// <base>_id or <base>_ids, or for a path parameter {id} the collection
// segment before it. Leading words are dropped until a model matches, so
// default_cluster_id finds Cluster.
func (b *builder) resolve(name, collection string) string {
	base := idBase(name)
	if base == "" && name == "id" {
		base = collection
	}
	if base == "" {
		return ""
	}
	words := strings.FieldsFunc(base, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
	for i := range words {
		for _, cand := range singulars(normKey(strings.Join(words[i:], ""))) {
			model, ok := b.flow.byKey[cand]
			if ok {
				return model
			}
		}
	}
	return ""
}

// idBase strips an id suffix: unit_id, unit_ids, unitId and unitIds give
// unit. Other names give "".
func idBase(name string) string {
	for _, suffix := range []string{"_ids", "_id", "Ids", "Id"} {
		if strings.HasSuffix(name, suffix) && len(name) > len(suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return ""
}

// singulars lists the forms of a normalized plural to try: amenities gives
// amenities, amenity, amenitie and amenitie's shorter forms.
func singulars(s string) []string {
	out := []string{s}
	if strings.HasSuffix(s, "ies") {
		out = append(out, strings.TrimSuffix(s, "ies")+"y")
	}
	if strings.HasSuffix(s, "es") {
		out = append(out, strings.TrimSuffix(s, "es"))
	}
	if strings.HasSuffix(s, "s") {
		out = append(out, strings.TrimSuffix(s, "s"))
	}
	return out
}

// normKey lowercases s and keeps only letters and digits.
func normKey(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func lastSegment(short string) string {
	i := strings.LastIndex(short, ".")
	return short[i+1:]
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// collectionOf returns the static path segment before {name}.
func collectionOf(path, name string) string {
	segs := pathSegments(path)
	for i, seg := range segs {
		if seg == "{"+name+"}" && i > 0 && !isParamSegment(segs[i-1]) {
			return segs[i-1]
		}
	}
	return ""
}

func (b *builder) buildMachine(ov *opView, of *opFlow) {
	ref := ov.ref
	m := machineView{Resource: resourceName(ref.Path)}

	consume := func(p *portView, model string) {
		if model == "" {
			return
		}
		p.Model = b.flow.ids[model]
		if !contains(of.consumed, model) {
			of.consumed = append(of.consumed, model)
			b.flow.consumers[model] = append(b.flow.consumers[model], ov)
		}
		var sources []*opView
		for _, src := range b.flow.producers[model] {
			if src != ov {
				sources = append(sources, src)
			}
		}
		if len(sources) > 0 {
			p.Source = sources[0]
			p.MoreSources = len(sources) - 1
		}
	}

	optional := portView{Key: "in:optional", Kind: "any"}
	for _, p := range b.d.Parameters(ref.Item, ref.Operation) {
		schema := parameterSchema(p)
		if !p.Required {
			optional.Names = append(optional.Names, p.Name)
			model := b.resolve(p.Name, "")
			if model != "" && !contains(of.consumed, model) {
				of.consumed = append(of.consumed, model)
				b.flow.consumers[model] = append(b.flow.consumers[model], ov)
			}
			continue
		}
		port := b.port(schema, ForRequest)
		port.Key = "in:" + p.In + ":" + p.Name
		port.Name = p.Name
		port.Required = true
		collection := ""
		if p.In == "path" {
			collection = collectionOf(ref.Path, p.Name)
		}
		consume(&port, b.resolve(p.Name, collection))
		m.Inputs = append(m.Inputs, port)
	}

	body := b.d.RequestBody(ref.Operation.RequestBody)
	if body != nil {
		var schema *Schema
		for _, ct := range body.Content.Keys {
			mt := body.Content.Map[ct]
			if mt != nil && mt.Schema != nil {
				schema = mt.Schema
				break
			}
		}
		if schema != nil {
			port := b.port(schema, ForRequest)
			port.Key = "in:body"
			port.Name = "body"
			port.Required = body.Required
			f := b.d.Flatten(schema)
			if f != nil {
				for _, key := range f.Properties.Keys {
					ps := f.Properties.Map[key]
					pf := b.d.Flatten(ps)
					if pf != nil && pf.ReadOnly {
						continue
					}
					model := b.resolve(key, "")
					if model == "" {
						continue
					}
					if len(port.Fields) == flowFieldsMax {
						port.MoreFields++
						consume(&portView{}, model)
						continue
					}
					field := b.port(ps, ForRequest)
					field.Key = "in:body:" + key
					field.Name = key
					consume(&field, model)
					port.Fields = append(port.Fields, field)
				}
			}
			m.Inputs = append(m.Inputs, port)
		}
	}

	if len(optional.Names) > 0 {
		optional.Name = strconv.Itoa(len(optional.Names)) + " optional"
		optional.Title = strings.Join(optional.Names, ", ")
		m.Inputs = append(m.Inputs, optional)
	}
	if len(m.Inputs) == 0 {
		m.Inputs = append(m.Inputs, portView{Key: "in:none", Name: "no inputs", Kind: "any", Ghost: true})
	}

	codes := append([]string(nil), ref.Operation.Responses.Keys...)
	sort.SliceStable(codes, func(i, j int) bool {
		return statusRank(codes[i]) < statusRank(codes[j])
	})
	for _, code := range codes {
		s := b.responseSchema(ref.Operation.Responses.Map[code])
		port := portView{Key: "out:" + code, Name: code, Class: statusClass(code), Main: strings.HasPrefix(code, "2"), Kind: "any", Type: "empty"}
		if s != nil {
			port = b.port(s, ForResponse)
			port.Key = "out:" + code
			port.Name = code
			port.Class = statusClass(code)
			port.Main = strings.HasPrefix(code, "2")
			names, list := b.produced(s)
			if list && len(names) > 0 {
				port.Type = b.short[names[0]] + "[]"
				port.Kind = "obj"
				port.Stack = true
			}
			var anchors []string
			for _, name := range names {
				if b.flow.ids[name] != "" {
					anchors = append(anchors, b.flow.ids[name])
				}
			}
			if port.Main {
				port.Model = strings.Join(anchors, " ")
			}
		}
		m.Outputs = append(m.Outputs, port)
	}

	ov.Machine = m
}

// resourceName is the last static segment of path, or / for the root.
func resourceName(path string) string {
	segs := pathSegments(path)
	for i := len(segs) - 1; i >= 0; i-- {
		if !isParamSegment(segs[i]) {
			return segs[i]
		}
	}
	return "/"
}

// port describes the type of s as a port: a short label such as int64,
// uuid, Amenity or Amenity[], and the shape class for its kind.
func (b *builder) port(s *Schema, mode ExampleMode) portView {
	f := b.d.Flatten(s)
	if f == nil {
		return portView{Kind: "any", Type: "any", Title: "any"}
	}
	p := portView{Title: b.newTree(mode, false).plainLabel(s)}
	name := b.d.SchemaName(s)
	switch {
	case isArray(f):
		item := b.port(f.Items, mode)
		p.Kind = item.Kind
		p.Type = item.Type + "[]"
		p.Stack = true
		if b.d.SchemaName(f.Items) != "" {
			p.Model = b.flow.ids[b.d.SchemaName(f.Items)]
		}
	case isObject(f) && name != "":
		p.Kind = "obj"
		p.Type = b.short[name]
		p.Model = b.flow.ids[name]
	case isObject(f):
		p.Kind = "obj"
		p.Type = "object"
	default:
		p.Type = f.Type.Primary()
		if f.Format != "" {
			p.Type = f.Format
		}
		if len(f.Enum) > 0 {
			p.Type = "enum"
		}
		switch f.Type.Primary() {
		case "string":
			p.Kind = "str"
		case "integer", "number":
			p.Kind = "num"
		case "boolean":
			p.Kind = "bool"
		default:
			p.Kind = "any"
			if p.Type == "" {
				p.Type = "any"
			}
		}
	}
	return p
}

// flowOp is one operation in the flow data the browser uses for recipes,
// source and user lists, and sidebar cards.
type flowOp struct {
	Anchor   string   `json:"a"`
	Method   string   `json:"m"`
	Title    string   `json:"t"`
	Path     string   `json:"p"`
	In       []string `json:"i,omitempty"`
	Optional int      `json:"q,omitempty"`
	Out      []string `json:"o,omitempty"`
	Errors   []string `json:"e,omitempty"`
}

type flowModel struct {
	Name   string `json:"n"`
	Anchor string `json:"a"`
	By     []int  `json:"by,omitempty"`
	Use    []int  `json:"use,omitempty"`
}

type flowData struct {
	Ops    []flowOp             `json:"ops"`
	Models map[string]flowModel `json:"models"`
}

func (b *builder) flowJSON(ops []*opView, flows map[*opView]*opFlow) template.JS {
	data := flowData{Models: map[string]flowModel{}}
	index := map[*opView]int{}
	for i, ov := range ops {
		index[ov] = i
		fo := flowOp{Anchor: ov.Anchor, Method: ov.Method, Title: ov.Title, Path: ov.Path}
		for _, p := range ov.Machine.Inputs {
			switch {
			case p.Ghost:
			case p.Key == "in:optional":
				fo.Optional = len(p.Names)
			case p.Key == "in:body":
				fo.In = append(fo.In, p.Type)
			default:
				fo.In = append(fo.In, p.Name)
			}
		}
		for _, p := range ov.Machine.Outputs {
			if p.Main {
				fo.Out = append(fo.Out, p.Type)
			} else {
				fo.Errors = append(fo.Errors, p.Name)
			}
		}
		data.Ops = append(data.Ops, fo)
	}

	add := func(name string, ov *opView, use bool) {
		id := b.flow.ids[name]
		if id == "" {
			return
		}
		fm := data.Models[id]
		fm.Name = b.short[name]
		fm.Anchor = b.models[name]
		if use {
			fm.Use = append(fm.Use, index[ov])
		} else {
			fm.By = append(fm.By, index[ov])
		}
		data.Models[id] = fm
	}
	for name, producers := range b.flow.producers {
		for _, ov := range producers {
			add(name, ov, false)
		}
	}
	for name, consumers := range b.flow.consumers {
		for _, ov := range consumers {
			add(name, ov, true)
		}
	}
	for id, fm := range data.Models {
		sort.Ints(fm.Use)
		data.Models[id] = fm
	}

	out, err := json.Marshal(data)
	if err != nil {
		return "{}"
	}
	return template.JS(out)
}
