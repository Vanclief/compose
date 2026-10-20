package reference

import (
	"sort"
	"strconv"
	"strings"
)

// ChangeKind says whether something was added, removed or changed between
// two versions of a document.
type ChangeKind int

const (
	Added ChangeKind = iota + 1
	Removed
	Changed
)

// String returns "added", "removed" or "changed".
func (k ChangeKind) String() string {
	switch k {
	case Added:
		return "added"
	case Removed:
		return "removed"
	case Changed:
		return "changed"
	}
	return ""
}

// Change is one difference between the base and the head document.
type Change struct {
	Kind ChangeKind
	// Path locates the change, segments joined by " › ", for example
	// "GET /issues/{id} › response 200 › created_at" or "Issue › status".
	Path string
	// Detail says what changed: "property removed", "type changed from
	// string to integer", "model Issue changed".
	Detail string
	// Breaking is set for changes that can break existing clients.
	Breaking bool

	// model is the component schema a "model changed" entry stands for, and
	// via the models with their own changes it changed through.
	model string
	via   []string
}

// inherited reports whether c only says that a referenced model changed.
func (c Change) inherited() bool {
	return c.model != "" && c.Kind == Changed
}

// OperationDiff is the result for one operation that was added, removed or
// changed. Operations are keyed by method and path.
type OperationDiff struct {
	Method   string
	Path     string
	Summary  string
	Tags     []string
	Kind     ChangeKind
	Breaking bool
	// Changes lists the operation's own changes, then one "model X
	// changed" entry per changed model it uses.
	Changes []Change
}

// Key returns "METHOD /path".
func (o *OperationDiff) Key() string {
	return o.Method + " " + o.Path
}

// ModelDiff is the result for one component schema that was added, removed
// or changed; its Changes end with a "model X changed" entry per changed
// model it references. A model also counts as changed when a model it
// references changed, so a change inside StandardError marks ErrorResponse
// and every operation that returns it.
type ModelDiff struct {
	Name     string
	Kind     ChangeKind
	Breaking bool
	Changes  []Change
}

// Report is the structural difference between a base (older) and a head
// (newer) OpenAPI document.
type Report struct {
	// Operations holds changed, added and removed operations in head
	// document order; removed operations follow the operation that preceded
	// them in the base document.
	Operations []*OperationDiff
	// Models holds changed, added and removed component schemas in head
	// order, then the removed ones in base order.
	Models []*ModelDiff
	// Changes is every change, operations first, then models.
	Changes []Change

	ops    map[string]*OperationDiff
	models map[string]*ModelDiff
	marks  *diffMarks
}

// Empty reports whether the documents have no differences.
func (r *Report) Empty() bool {
	return len(r.Changes) == 0
}

// Breaking reports whether any change is breaking.
func (r *Report) Breaking() bool {
	for _, c := range r.Changes {
		if c.Breaking {
			return true
		}
	}
	return false
}

// Operation returns the result for method and path, or nil when the
// operation is the same in both documents.
func (r *Report) Operation(method, path string) *OperationDiff {
	return r.ops[strings.ToUpper(method)+" "+path]
}

// Model returns the result for a component schema, or nil when it is the
// same in both documents.
func (r *Report) Model(name string) *ModelDiff {
	return r.models[name]
}

const pathSep = " › "

// The schema comparison runs in one of two contexts, because the same change
// breaks clients in one direction only: a removed property breaks readers of
// a response, a new required property breaks writers of a request.
type diffCtx int

const (
	ctxModel diffCtx = iota
	ctxRequest
	ctxResponse
)

type rawChange struct {
	kind      ChangeKind
	path      []string
	detail    string
	breakReq  bool
	breakResp bool
	ctx       diffCtx
}

func (c rawChange) breaking(ctx diffCtx) bool {
	switch ctx {
	case ctxRequest:
		return c.breakReq
	case ctxResponse:
		return c.breakResp
	}
	return c.breakReq || c.breakResp
}

// refUse is a $ref to the same named model on both sides. The model is
// compared once on its own; the reference only inherits its result.
type refUse struct {
	name string
	path []string
	ctx  diffCtx
}

// diffMarks tells the schema tree how to mark its rows, keyed by the head
// document's own property and parameter schema pointers, which are stable
// across Flatten calls and unique per location.
type diffMarks struct {
	rows map[*Schema]*rowMark
	// after and before hold removed properties, written after the head
	// property that preceded them in the base, or before the first head
	// property when nothing did.
	after  map[*Schema][]rowSpec
	before map[*Schema][]rowSpec
	// params holds removed parameters per head operation.
	params map[*Operation][]removedParam
}

type rowMark struct {
	added bool
	// old is the base version of a row whose type, format, nullable,
	// required, description or flags changed; it is written before the row.
	old *rowSpec
	// enum is set when the enum values changed.
	enum *enumMark
	// inside is set when only nested properties or items changed.
	inside bool
}

type enumMark struct {
	was []any
}

type removedParam struct {
	in  string
	row rowSpec
}

func newDiffMarks() *diffMarks {
	return &diffMarks{
		rows:   map[*Schema]*rowMark{},
		after:  map[*Schema][]rowSpec{},
		before: map[*Schema][]rowSpec{},
		params: map[*Operation][]removedParam{},
	}
}

// mark returns the mark of a head row, creating it. A row that is already
// marked keeps its first mark, so a schema shared through YAML aliases or
// component parameters is not marked twice.
func (m *diffMarks) mark(s *Schema) *rowMark {
	rm, ok := m.rows[s]
	if !ok {
		rm = &rowMark{}
		m.rows[s] = rm
	}
	return rm
}

type modelState struct {
	changes []rawChange
	uses    []refUse
}

type differ struct {
	base, head *Document
	marks      *diffMarks
	models     map[string]*modelState
	deep       map[string]bool
	deepBreak  map[string]bool
}

// walk collects the changes of one operation or model.
type walk struct {
	d       *differ
	ctx     diffCtx
	changes []rawChange
	uses    []refUse
	// pairs guards structural comparisons across differently named models
	// against cycles.
	pairs map[[2]*Schema]bool
}

// Diff compares two documents structurally: operations by method and path,
// schemas property by property after allOf flattening and ref resolution.
// References to a model of the same name on both sides are not expanded;
// the model is compared once and every operation or model that reaches a
// changed model through references is reported as changed.
//
// Breaking changes follow oasdiff: a removed operation; a removed (or
// renamed) response property; a new required request property or parameter;
// a type change; enum values removed from a request; an existing request
// property or parameter made required; a response property made nullable.
// Everything else is a non-breaking change.
func Diff(base, head *Document) *Report {
	if base == nil {
		base = &Document{}
	}
	if head == nil {
		head = &Document{}
	}
	d := &differ{
		base:      base,
		head:      head,
		marks:     newDiffMarks(),
		models:    map[string]*modelState{},
		deep:      map[string]bool{},
		deepBreak: map[string]bool{},
	}
	r := &Report{ops: map[string]*OperationDiff{}, models: map[string]*ModelDiff{}, marks: d.marks}

	// Models first, so operations can inherit their results.
	var modelOrder []string
	modelKinds := map[string]ChangeKind{}
	for _, name := range head.Components.Schemas.Keys {
		modelOrder = append(modelOrder, name)
		bs, ok := base.Components.Schemas.Get(name)
		if !ok {
			modelKinds[name] = Added
			continue
		}
		hs := head.Components.Schemas.Map[name]
		w := d.newWalk(ctxModel)
		res := w.schema(bs, hs, []string{name}, true)
		// A model that is an enum shows its values in the card head.
		if res.enum && hs != nil {
			d.marks.mark(hs).enum = &enumMark{was: enumOf(base, base.Flatten(bs))}
		}
		d.models[name] = &modelState{changes: w.changes, uses: w.uses}
	}
	for _, name := range base.Components.Schemas.Keys {
		_, ok := head.Components.Schemas.Get(name)
		if !ok {
			modelOrder = append(modelOrder, name)
			modelKinds[name] = Removed
		}
	}

	usage := map[string]map[diffCtx]bool{}
	for _, od := range d.operations() {
		for _, u := range od.uses {
			d.markUsage(u.name, u.ctx, usage, map[string]bool{})
		}
		if od.diff == nil {
			continue
		}
		r.Operations = append(r.Operations, od.diff)
		r.ops[od.diff.Key()] = od.diff
	}

	for _, name := range modelOrder {
		md := d.modelDiff(name, modelKinds[name], usage[name])
		if md == nil {
			continue
		}
		r.Models = append(r.Models, md)
		r.models[name] = md
	}

	for _, od := range r.Operations {
		r.Changes = append(r.Changes, od.Changes...)
	}
	for _, md := range r.Models {
		r.Changes = append(r.Changes, md.Changes...)
	}
	return r
}

func (d *differ) newWalk(ctx diffCtx) *walk {
	return &walk{d: d, ctx: ctx, pairs: map[[2]*Schema]bool{}}
}

// markUsage records that name is read in ctx, directly or through models
// that reference it.
func (d *differ) markUsage(name string, ctx diffCtx, usage map[string]map[diffCtx]bool, seen map[string]bool) {
	if seen[name] {
		return
	}
	seen[name] = true
	if usage[name] == nil {
		usage[name] = map[diffCtx]bool{}
	}
	usage[name][ctx] = true
	st := d.models[name]
	if st == nil {
		return
	}
	for _, u := range st.uses {
		d.markUsage(u.name, ctx, usage, seen)
	}
}

// changed reports whether model name changed itself or through a model it
// references.
func (d *differ) changed(name string) bool {
	v, ok := d.deep[name]
	if ok {
		return v
	}
	v = d.reach(name, map[string]bool{}, func(st *modelState) bool {
		return len(st.changes) > 0
	})
	d.deep[name] = v
	return v
}

// breaks reports whether model name has a change, itself or through
// references, that is breaking in ctx.
func (d *differ) breaks(name string, ctx diffCtx) bool {
	key := name + "\x00" + strconv.Itoa(int(ctx))
	v, ok := d.deepBreak[key]
	if ok {
		return v
	}
	v = d.reach(name, map[string]bool{}, func(st *modelState) bool {
		for _, c := range st.changes {
			if c.breaking(ctx) {
				return true
			}
		}
		return false
	})
	d.deepBreak[key] = v
	return v
}

// sources lists, sorted, the models with changes of their own that name
// reaches through references.
func (d *differ) sources(name string) []string {
	var out []string
	seen := map[string]bool{}
	var visit func(string)
	visit = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		st := d.models[n]
		if st == nil {
			return
		}
		if len(st.changes) > 0 {
			out = append(out, n)
		}
		for _, u := range st.uses {
			visit(u.name)
		}
	}
	visit(name)
	sort.Strings(out)
	return out
}

func (d *differ) reach(name string, seen map[string]bool, own func(*modelState) bool) bool {
	if seen[name] {
		return false
	}
	seen[name] = true
	st := d.models[name]
	if st == nil {
		return false
	}
	if own(st) {
		return true
	}
	for _, u := range st.uses {
		if d.reach(u.name, seen, own) {
			return true
		}
	}
	return false
}

// useChanges turns the references of a walk into "model X changed" entries
// for the models that changed.
func (d *differ) useChanges(uses []refUse) []Change {
	var out []Change
	seen := map[string]bool{}
	for _, u := range uses {
		if !d.changed(u.name) {
			continue
		}
		path := strings.Join(u.path, pathSep)
		key := path + "\x00" + u.name
		if seen[key] {
			continue
		}
		seen[key] = true
		c := Change{
			Kind:     Changed,
			Path:     path,
			Detail:   "model " + u.name + " changed",
			Breaking: d.breaks(u.name, u.ctx),
			model:    u.name,
		}
		st := d.models[u.name]
		if st != nil && len(st.changes) == 0 {
			c.via = d.sources(u.name)
			c.Detail += " (through " + strings.Join(c.via, ", ") + ")"
		}
		out = append(out, c)
	}
	return out
}

func (d *differ) modelDiff(name string, kind ChangeKind, usage map[diffCtx]bool) *ModelDiff {
	switch kind {
	case Added:
		return &ModelDiff{Name: name, Kind: Added, Changes: []Change{{Kind: Added, Path: name, Detail: "model added", model: name}}}
	case Removed:
		return &ModelDiff{Name: name, Kind: Removed, Changes: []Change{{Kind: Removed, Path: name, Detail: "model removed", model: name}}}
	}

	st := d.models[name]
	if st == nil || !d.changed(name) {
		return nil
	}
	md := &ModelDiff{Name: name, Kind: Changed}
	for _, c := range st.changes {
		breaking := c.breaking(ctxModel)
		if len(usage) > 0 {
			breaking = usage[ctxRequest] && c.breakReq || usage[ctxResponse] && c.breakResp
		}
		md.Changes = append(md.Changes, Change{Kind: c.kind, Path: strings.Join(c.path, pathSep), Detail: c.detail, Breaking: breaking})
	}
	for _, c := range d.useChanges(st.uses) {
		if len(usage) > 0 {
			c.Breaking = usage[ctxRequest] && d.breaks(c.model, ctxRequest) || usage[ctxResponse] && d.breaks(c.model, ctxResponse)
		}
		md.Changes = append(md.Changes, c)
	}
	for _, c := range md.Changes {
		if c.Breaking {
			md.Breaking = true
		}
	}
	return md
}

type opResult struct {
	diff *OperationDiff
	uses []refUse
}

// operations compares every operation in head order, each removed one after
// its base predecessor. Unchanged operations come back without a diff, for
// their model usage only.
func (d *differ) operations() []opResult {
	baseOps := map[string]OperationRef{}
	for _, ref := range d.base.Operations() {
		baseOps[ref.Method+" "+ref.Path] = ref
	}
	headOps := map[string]bool{}
	for _, ref := range d.head.Operations() {
		headOps[ref.Method+" "+ref.Path] = true
	}

	// Removed operations follow the closest preceding base operation that
	// still exists.
	removedAfter := map[string][]OperationRef{}
	prev := ""
	for _, ref := range d.base.Operations() {
		key := ref.Method + " " + ref.Path
		if headOps[key] {
			prev = key
			continue
		}
		removedAfter[prev] = append(removedAfter[prev], ref)
	}

	var out []opResult
	appendRemoved := func(after string) {
		for _, ref := range removedAfter[after] {
			key := ref.Method + " " + ref.Path
			od := &OperationDiff{
				Method:   ref.Method,
				Path:     ref.Path,
				Summary:  ref.Operation.Summary,
				Tags:     ref.Operation.Tags,
				Kind:     Removed,
				Breaking: true,
				Changes:  []Change{{Kind: Removed, Path: key, Detail: "operation removed", Breaking: true}},
			}
			out = append(out, opResult{diff: od})
		}
	}

	appendRemoved("")
	for _, ref := range d.head.Operations() {
		key := ref.Method + " " + ref.Path
		bref, ok := baseOps[key]
		od := &OperationDiff{Method: ref.Method, Path: ref.Path, Summary: ref.Operation.Summary, Tags: ref.Operation.Tags}
		if !ok {
			od.Kind = Added
			od.Changes = []Change{{Kind: Added, Path: key, Detail: "operation added"}}
			out = append(out, opResult{diff: od})
			appendRemoved(key)
			continue
		}

		w := d.newWalk(ctxRequest)
		w.operation(bref, ref, key)
		for _, c := range w.changes {
			od.Changes = append(od.Changes, Change{Kind: c.kind, Path: strings.Join(c.path, pathSep), Detail: c.detail, Breaking: c.breaking(c.ctx)})
		}
		od.Changes = append(od.Changes, d.useChanges(w.uses)...)
		if len(od.Changes) > 0 {
			od.Kind = Changed
			for _, c := range od.Changes {
				if c.Breaking {
					od.Breaking = true
				}
			}
			out = append(out, opResult{diff: od, uses: w.uses})
		} else {
			out = append(out, opResult{uses: w.uses})
		}
		appendRemoved(key)
	}

	return out
}

func (w *walk) add(kind ChangeKind, path []string, detail string, breakReq, breakResp bool) {
	w.changes = append(w.changes, rawChange{kind: kind, path: path, detail: detail, breakReq: breakReq, breakResp: breakResp, ctx: w.ctx})
}

func extend(path []string, segs ...string) []string {
	out := make([]string, 0, len(path)+len(segs))
	out = append(out, path...)
	return append(out, segs...)
}

// itemsPath appends [] to a property name, tags becoming tags[], and an
// items segment to a body or response: response 200 › items.
func itemsPath(path []string) []string {
	out := append([]string(nil), path...)
	if len(out) == 0 || strings.Contains(out[len(out)-1], " ") {
		return append(out, "items")
	}
	out[len(out)-1] += "[]"
	return out
}

func (w *walk) operation(bref, href OperationRef, key string) {
	bop := bref.Operation
	hop := href.Operation
	root := []string{key}

	w.ctx = ctxRequest
	if bop.Deprecated != hop.Deprecated {
		detail := "operation deprecated"
		if !hop.Deprecated {
			detail = "operation no longer deprecated"
		}
		w.add(Changed, root, detail, false, false)
	}
	bsec := securityKey(w.d.base.OperationSecurity(bop))
	hsec := securityKey(w.d.head.OperationSecurity(hop))
	if bsec != hsec {
		w.add(Changed, root, "security changed from "+securityText(w.d.base.OperationSecurity(bop))+" to "+securityText(w.d.head.OperationSecurity(hop)), false, false)
	}

	w.parameters(bref, href, root)
	w.requestBody(w.d.base.RequestBody(bop.RequestBody), w.d.head.RequestBody(hop.RequestBody), root)

	w.ctx = ctxResponse
	for _, code := range bop.Responses.Keys {
		_, ok := hop.Responses.Get(code)
		if !ok {
			w.add(Removed, extend(root, "response "+code), "response removed", false, false)
		}
	}
	for _, code := range hop.Responses.Keys {
		br, ok := bop.Responses.Get(code)
		path := extend(root, "response "+code)
		if !ok {
			w.add(Added, path, "response added", false, false)
			continue
		}
		bresp := w.d.base.Response(br)
		hresp := w.d.head.Response(hop.Responses.Map[code])
		if bresp == nil || hresp == nil {
			continue
		}
		w.content(bresp.Content, hresp.Content, path)
	}
}

func securityText(reqs []map[string][]string) string {
	if len(reqs) == 0 {
		return "none"
	}
	var sets []string
	for _, req := range reqs {
		names := make([]string, 0, len(req))
		for name := range req {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) == 0 {
			sets = append(sets, "optional")
			continue
		}
		sets = append(sets, strings.Join(names, " + "))
	}
	return strings.Join(sets, " or ")
}

func (w *walk) parameters(bref, href OperationRef, root []string) {
	key := func(p *Parameter) string {
		return p.In + "\x00" + p.Name
	}
	bparams := w.d.base.Parameters(bref.Item, bref.Operation)
	hparams := w.d.head.Parameters(href.Item, href.Operation)
	byKey := map[string]*Parameter{}
	for _, p := range hparams {
		byKey[key(p)] = p
	}
	baseKey := map[string]*Parameter{}
	for _, p := range bparams {
		baseKey[key(p)] = p
	}

	for _, bp := range bparams {
		_, ok := byKey[key(bp)]
		if ok {
			continue
		}
		w.add(Removed, extend(root, bp.In, bp.Name), "parameter removed", false, false)
		row := rowSpec{name: bp.Name, schema: parameterSchema(bp), required: bp.Required, description: bp.Description, deprecated: bp.Deprecated, param: true, mark: markDel}
		w.d.marks.params[href.Operation] = append(w.d.marks.params[href.Operation], removedParam{in: bp.In, row: row})
	}

	for _, hp := range hparams {
		path := extend(root, hp.In, hp.Name)
		hs := parameterSchema(hp)
		bp, ok := baseKey[key(hp)]
		if !ok {
			detail := "parameter added"
			if hp.Required {
				detail = "required parameter added"
			}
			w.add(Added, path, detail, hp.Required, false)
			if hs != nil {
				w.d.marks.mark(hs).added = true
			}
			continue
		}

		head := false
		if !bp.Required && hp.Required {
			w.add(Changed, path, "parameter made required", true, false)
			head = true
		}
		if bp.Required && !hp.Required {
			w.add(Changed, path, "parameter made optional", false, false)
			head = true
		}
		if bp.Description != hp.Description {
			w.add(Changed, path, "description changed", false, false)
			head = true
		}
		if bp.Deprecated != hp.Deprecated {
			w.add(Changed, path, deprecationText(hp.Deprecated), false, false)
			head = true
		}
		bs := parameterSchema(bp)
		res := w.schema(bs, hs, path, true)
		if hs == nil {
			continue
		}
		w.markRow(hs, rowSpec{name: bp.Name, schema: bs, required: bp.Required, description: bp.Description, deprecated: bp.Deprecated, param: true}, head || res.head, res, bs)
	}
}

func deprecationText(deprecated bool) string {
	if deprecated {
		return "deprecated"
	}
	return "no longer deprecated"
}

func (w *walk) requestBody(bb, hb *RequestBody, root []string) {
	path := extend(root, "request body")
	switch {
	case bb == nil && hb == nil:
		return
	case bb == nil:
		w.add(Added, path, "request body added", hb.Required, false)
		return
	case hb == nil:
		w.add(Removed, path, "request body removed", false, false)
		return
	}
	if !bb.Required && hb.Required {
		w.add(Changed, path, "request body made required", true, false)
	}
	if bb.Required && !hb.Required {
		w.add(Changed, path, "request body made optional", false, false)
	}
	w.content(bb.Content, hb.Content, path)
}

// content compares the media types of a body or response. Matching content
// types are compared by schema; with one type on each side the two are
// compared even when the type changed.
func (w *walk) content(bc, hc Ordered[*MediaType], path []string) {
	for _, ct := range bc.Keys {
		_, ok := hc.Get(ct)
		if !ok {
			w.add(Removed, path, "content type "+ct+" removed", false, false)
		}
	}
	for _, ct := range hc.Keys {
		_, ok := bc.Get(ct)
		if !ok {
			w.add(Added, path, "content type "+ct+" added", false, false)
		}
	}

	pairs := [][2]string{}
	for _, ct := range hc.Keys {
		_, ok := bc.Get(ct)
		if ok {
			pairs = append(pairs, [2]string{ct, ct})
		}
	}
	if len(pairs) == 0 && bc.Len() == 1 && hc.Len() == 1 {
		pairs = append(pairs, [2]string{bc.Keys[0], hc.Keys[0]})
	}
	for _, p := range pairs {
		bm := bc.Map[p[0]]
		hm := hc.Map[p[1]]
		var bs, hs *Schema
		if bm != nil {
			bs = bm.Schema
		}
		if hm != nil {
			hs = hm.Schema
		}
		sub := path
		if len(pairs) > 1 {
			sub = extend(path, p[1])
		}
		w.schema(bs, hs, sub, true)
	}
}

// schemaResult says what a property row needs to show for a schema change.
type schemaResult struct {
	head   bool // type, format, nullable, description or a flag changed
	enum   bool // enum values changed (the schema's own or its items')
	inside bool // nested properties, items or variants changed
}

// schema compares b with h and records the differences under path. rec
// enables row marks; it is turned off below a head schema that belongs to
// another model than the base, whose rows are marked by that model's own
// comparison.
func (w *walk) schema(b, h *Schema, path []string, rec bool) schemaResult {
	var res schemaResult
	if b == nil && h == nil {
		return res
	}
	if b == nil || h == nil {
		detail := "schema added"
		kind := Added
		if h == nil {
			detail = "schema removed"
			kind = Removed
		}
		w.add(kind, path, detail, false, false)
		res.head = true
		return res
	}

	bn := w.d.base.SchemaName(b)
	hn := w.d.head.SchemaName(h)
	if hn != "" && bn == hn {
		res.head = w.wrapper(b, h, path)
		w.uses = append(w.uses, refUse{name: hn, path: path, ctx: w.ctx})
		return res
	}
	if bn != hn {
		if bn != "" && hn != "" {
			w.add(Changed, path, "schema changed from "+bn+" to "+hn, false, false)
			res.head = true
		}
		if hn != "" {
			rec = false
		}
	}

	br, _ := w.d.base.Schema(b)
	hr, _ := w.d.head.Schema(h)
	pair := [2]*Schema{br, hr}
	if w.pairs[pair] {
		return res
	}
	w.pairs[pair] = true
	defer delete(w.pairs, pair)

	bf := w.d.base.Flatten(b)
	hf := w.d.head.Flatten(h)
	if bf == nil || hf == nil {
		return res
	}

	bt := typeText(bf)
	ht := typeText(hf)
	if bt != ht {
		w.add(Changed, path, "type changed from "+quoteType(bt)+" to "+quoteType(ht), true, true)
		res.head = true
	}
	if bf.Format != hf.Format {
		w.add(Changed, path, formatText(bf.Format, hf.Format), false, false)
		res.head = true
	}
	if bf.Nullable != hf.Nullable {
		if hf.Nullable {
			w.add(Changed, path, "made nullable", false, true)
		} else {
			w.add(Changed, path, "no longer nullable", false, false)
		}
		res.head = true
	}
	if bf.Description != hf.Description {
		w.add(Changed, path, "description changed", false, false)
		res.head = true
	}
	if bf.Deprecated != hf.Deprecated {
		w.add(Changed, path, deprecationText(hf.Deprecated), false, false)
		res.head = true
	}
	if bf.ReadOnly != hf.ReadOnly {
		w.add(Changed, path, flagText("read-only", hf.ReadOnly), false, false)
		res.head = true
	}
	if bf.WriteOnly != hf.WriteOnly {
		w.add(Changed, path, flagText("write-only", hf.WriteOnly), false, false)
		res.head = true
	}

	added, removed := enumDelta(bf.Enum, hf.Enum)
	if len(added) > 0 || len(removed) > 0 {
		w.add(Changed, path, enumText(added, removed), len(removed) > 0, false)
		res.enum = true
	}

	if bf.Items != nil || hf.Items != nil {
		if bf.Items != nil && hf.Items != nil {
			sub := w.schema(bf.Items, hf.Items, itemsPath(path), rec)
			res.head = res.head || sub.head
			res.enum = res.enum || sub.enum
			res.inside = res.inside || sub.inside
		} else if bt == ht {
			w.add(Changed, itemsPath(path), "items changed", false, false)
			res.inside = true
		}
	}

	if w.properties(bf, hf, path, rec) {
		res.inside = true
	}

	ba := bf.AdditionalSchema()
	ha := hf.AdditionalSchema()
	if ba != nil && ha != nil {
		sub := w.schema(ba, ha, extend(path, "<key>"), false)
		if sub.head || sub.enum || sub.inside {
			res.inside = true
		}
	} else if (ba == nil) != (ha == nil) {
		w.add(Changed, path, "additional properties changed", false, false)
		res.inside = true
	}

	if w.variants("one of", bf.OneOf, hf.OneOf, path, rec) {
		res.inside = true
	}
	if w.variants("any of", bf.AnyOf, hf.AnyOf, path, rec) {
		res.inside = true
	}
	return res
}

// wrapper compares the keywords a reference carries next to the model it
// points at, such as an allOf wrapper that adds nullable or a description.
func (w *walk) wrapper(b, h *Schema, path []string) bool {
	changed := false
	if b.Description != h.Description {
		w.add(Changed, path, "description changed", false, false)
		changed = true
	}
	if b.Nullable != h.Nullable {
		if h.Nullable {
			w.add(Changed, path, "made nullable", false, true)
		} else {
			w.add(Changed, path, "no longer nullable", false, false)
		}
		changed = true
	}
	if b.Deprecated != h.Deprecated {
		w.add(Changed, path, deprecationText(h.Deprecated), false, false)
		changed = true
	}
	if b.ReadOnly != h.ReadOnly {
		w.add(Changed, path, flagText("read-only", h.ReadOnly), false, false)
		changed = true
	}
	if b.WriteOnly != h.WriteOnly {
		w.add(Changed, path, flagText("write-only", h.WriteOnly), false, false)
		changed = true
	}
	return changed
}

func flagText(flag string, set bool) string {
	if set {
		return "made " + flag
	}
	return "no longer " + flag
}

// properties compares the properties of two flattened objects and reports
// whether any differ.
func (w *walk) properties(bf, hf *Schema, path []string, rec bool) bool {
	changed := false
	marks := w.d.marks

	for i, key := range bf.Properties.Keys {
		_, ok := hf.Properties.Get(key)
		if ok {
			continue
		}
		changed = true
		w.add(Removed, extend(path, key), "property removed", false, true)
		if !rec || hf.Properties.Len() == 0 {
			continue
		}
		row := rowSpec{name: key, schema: bf.Properties.Map[key], required: bf.IsRequired(key), mark: markDel}
		anchor := w.predecessor(bf, hf, i)
		if anchor != nil {
			marks.after[anchor] = append(marks.after[anchor], row)
		} else {
			first := hf.Properties.Map[hf.Properties.Keys[0]]
			marks.before[first] = append(marks.before[first], row)
		}
	}

	for _, key := range hf.Properties.Keys {
		hp := hf.Properties.Map[key]
		bp, ok := bf.Properties.Get(key)
		path := extend(path, key)
		required := hf.IsRequired(key)
		if !ok {
			changed = true
			detail := "property added"
			if required {
				detail = "required property added"
			}
			w.add(Added, path, detail, required, false)
			if rec && hp != nil {
				marks.mark(hp).added = true
			}
			continue
		}

		head := false
		wasRequired := bf.IsRequired(key)
		if !wasRequired && required {
			w.add(Changed, path, "made required", true, false)
			head = true
		}
		if wasRequired && !required {
			w.add(Changed, path, "made optional", false, false)
			head = true
		}
		res := w.schema(bp, hp, path, rec)
		if head || res.head || res.enum || res.inside {
			changed = true
		}
		if rec && hp != nil {
			w.markRow(hp, rowSpec{name: key, schema: bp, required: wasRequired}, head || res.head, res, bp)
		}
	}
	return changed
}

// predecessor returns the head property that precedes base property i: the
// closest earlier base property that still exists.
func (w *walk) predecessor(bf, hf *Schema, i int) *Schema {
	for j := i - 1; j >= 0; j-- {
		hp, ok := hf.Properties.Get(bf.Properties.Keys[j])
		if ok {
			return hp
		}
	}
	return nil
}

// markRow records how a row present on both sides shows its change.
func (w *walk) markRow(hs *Schema, old rowSpec, head bool, res schemaResult, bs *Schema) {
	if !head && !res.enum && !res.inside {
		return
	}
	m := w.d.marks.mark(hs)
	if m.added || m.old != nil || m.enum != nil || m.inside {
		return
	}
	if head {
		old.mark = markDel
		m.old = &old
	}
	if res.enum {
		m.enum = &enumMark{was: enumOf(w.d.base, w.d.base.Flatten(bs))}
	}
	if !head && !res.enum {
		m.inside = true
	}
}

// variants compares oneOf or anyOf members by position.
func (w *walk) variants(label string, bl, hl []*Schema, path []string, rec bool) bool {
	if len(bl) == 0 && len(hl) == 0 {
		return false
	}
	if len(bl) != len(hl) {
		w.add(Changed, path, label+" variants changed from "+strconv.Itoa(len(bl))+" to "+strconv.Itoa(len(hl)), false, false)
		return true
	}
	changed := false
	for i := range bl {
		res := w.schema(bl[i], hl[i], extend(path, label+" #"+strconv.Itoa(i+1)), rec)
		if res.head || res.enum || res.inside {
			changed = true
		}
	}
	return changed
}

// typeText is the non-null type list of f, inferring object and array from
// properties and items when no type is declared.
func typeText(f *Schema) string {
	var types []string
	for _, t := range f.Type {
		if t != "null" {
			types = append(types, t)
		}
	}
	if len(types) == 0 {
		switch {
		case isObject(f):
			return "object"
		case f.Items != nil:
			return "array"
		}
		return ""
	}
	return strings.Join(types, " | ")
}

func formatText(base, head string) string {
	switch {
	case base == "":
		return "format " + head + " added"
	case head == "":
		return "format " + base + " removed"
	}
	return "format changed from " + base + " to " + head
}

func quoteType(t string) string {
	if t == "" {
		return "any"
	}
	return t
}

// enumOf returns the enum a row shows: its own or its items'.
func enumOf(d *Document, f *Schema) []any {
	if f == nil {
		return nil
	}
	if len(f.Enum) > 0 {
		return f.Enum
	}
	if isArray(f) && f.Items != nil {
		items := d.Flatten(f.Items)
		if items != nil {
			return items.Enum
		}
	}
	return nil
}

func enumKey(v any) string {
	return jsonValue(v)
}

// enumDelta returns the values of head missing from base and the values of
// base missing from head.
func enumDelta(base, head []any) ([]any, []any) {
	inBase := map[string]bool{}
	for _, v := range base {
		inBase[enumKey(v)] = true
	}
	inHead := map[string]bool{}
	var added []any
	for _, v := range head {
		inHead[enumKey(v)] = true
		if !inBase[enumKey(v)] {
			added = append(added, v)
		}
	}
	var removed []any
	for _, v := range base {
		if !inHead[enumKey(v)] {
			removed = append(removed, v)
		}
	}
	return added, removed
}

func enumText(added, removed []any) string {
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "enum values added: "+enumList(added))
	}
	if len(removed) > 0 {
		parts = append(parts, "enum values removed: "+enumList(removed))
	}
	return strings.Join(parts, "; ")
}

func enumList(values []any) string {
	var out []string
	for _, v := range values {
		out = append(out, jsonValue(v))
	}
	return strings.Join(out, ", ")
}

// Markdown renders the report as a changelog for release notes or CI:
// breaking changes first, then the rest, each grouped by the first tag of
// the operation, with model changes under Models.
func (r *Report) Markdown() string {
	var sb strings.Builder
	sb.WriteString("# API changes\n\n")
	if r.Empty() {
		sb.WriteString("No changes.\n")
		return sb.String()
	}

	breaking := 0
	for _, c := range r.Changes {
		if c.Breaking {
			breaking++
		}
	}
	sb.WriteString(strconv.Itoa(breaking) + " breaking and " + strconv.Itoa(len(r.Changes)-breaking) + " non-breaking " + plural(len(r.Changes), "change", "changes") + " in " +
		strconv.Itoa(len(r.Operations)) + " " + plural(len(r.Operations), "operation", "operations") + " and " +
		strconv.Itoa(len(r.Models)) + " " + plural(len(r.Models), "model", "models") + ".\n")

	if breaking > 0 {
		sb.WriteString("\n## Breaking changes\n")
		r.markdownGroups(&sb, true)
	}
	if breaking < len(r.Changes) {
		sb.WriteString("\n## Other changes\n")
		r.markdownGroups(&sb, false)
	}
	return sb.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// changeGroup is the changes of one tag, or of the models, for Markdown.
type changeGroup struct {
	name    string
	changes []Change
}

// groups collects the breaking or the non-breaking changes by the first tag
// of their operation, in report order, then the model changes.
func (r *Report) groups(breaking bool) []*changeGroup {
	var out []*changeGroup
	byName := map[string]*changeGroup{}
	for _, od := range r.Operations {
		tag := "Other"
		if len(od.Tags) > 0 {
			tag = od.Tags[0]
		}
		for _, c := range od.Changes {
			if c.Breaking != breaking {
				continue
			}
			g := byName[tag]
			if g == nil {
				g = &changeGroup{name: tag}
				byName[tag] = g
				out = append(out, g)
			}
			g.changes = append(g.changes, c)
		}
	}
	// A model that only changed through another one is left out: the other
	// model's own entries say what changed.
	models := &changeGroup{name: "Models"}
	for _, md := range r.Models {
		for _, c := range md.Changes {
			if c.Breaking == breaking && !c.inherited() {
				models.changes = append(models.changes, c)
			}
		}
	}
	if len(models.changes) > 0 {
		out = append(out, models)
	}
	return out
}

func (r *Report) markdownGroups(sb *strings.Builder, breaking bool) {
	for _, g := range r.groups(breaking) {
		sb.WriteString("\n### " + g.name + "\n\n")
		for _, c := range g.changes {
			sb.WriteString("- " + markdownChange(c) + "\n")
		}
	}
}

// markdownChange writes a change as `GET /x` › response 200 › id: detail.
func markdownChange(c Change) string {
	segs := strings.Split(c.Path, pathSep)
	out := "`" + segs[0] + "`"
	if len(segs) > 1 {
		out += pathSep + strings.Join(segs[1:], pathSep)
	}
	detail := c.Detail
	for _, name := range append([]string{c.model}, c.via...) {
		if name != "" {
			detail = strings.Replace(detail, name, "`"+name+"`", 1)
		}
	}
	return out + ": " + detail
}
