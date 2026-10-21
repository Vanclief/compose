package reference

import (
	"bytes"
	"encoding/json"
	"html/template"
	"sort"
	"strconv"
	"strings"

	"github.com/vanclief/ez"
)

// Diff mode: when Options.Base is set the page is the head document marked
// up with what changed since the base. Everything the view needs beyond the
// plain page lives here; render.go and view.go only call in.

// Row mark classes.
const (
	markAdd = "diff-add"
	markDel = "diff-del"
	markChg = "diff-chg"
)

type diffState struct {
	base    *Document
	report  *Report
	removed map[*Operation]bool // base operations shown as removed
	samples []*sampleDiffView   // the response sample diffs, see sampleDiffs
}

// newDiffState parses base and compares it with head; it returns nil when
// base is nil.
func newDiffState(head *Document, base []byte) (*diffState, error) {
	if base == nil {
		return nil, nil
	}
	doc, err := Parse(base)
	if err != nil {
		return nil, ez.New(ez.ErrorCode(err), "reference: base document: "+ez.ErrorMessage(err), err)
	}

	st := &diffState{base: doc, report: Diff(doc, head), removed: map[*Operation]bool{}}
	for _, ref := range doc.Operations() {
		od := st.report.Operation(ref.Method, ref.Path)
		if od != nil && od.Kind == Removed {
			st.removed[ref.Operation] = true
		}
	}
	return st, nil
}

// diffBadge is the change badge of an operation or model.
type diffBadge struct {
	Class    string // diff-added, diff-changed or diff-removed
	Label    string // ADDED, CHANGED or REMOVED
	Letter   string // A, C or R, for the sidebar
	Breaking bool
	op       *OperationDiff
}

func newBadge(kind ChangeKind, breaking bool) *diffBadge {
	switch kind {
	case Added:
		return &diffBadge{Class: "diff-added", Label: "ADDED", Letter: "A", Breaking: breaking}
	case Removed:
		return &diffBadge{Class: "diff-removed", Label: "REMOVED", Letter: "R", Breaking: breaking}
	}
	return &diffBadge{Class: "diff-changed", Label: "CHANGED", Letter: "C", Breaking: breaking}
}

// pageOperations lists the operations the page shows: the head document's
// and, in diff mode, the removed ones after the operation that preceded
// them in the base.
func (b *builder) pageOperations() []OperationRef {
	refs := b.d.Operations()
	if b.diff == nil || len(b.diff.removed) == 0 {
		return refs
	}

	inHead := map[string]bool{}
	for _, ref := range refs {
		inHead[ref.Method+" "+ref.Path] = true
	}
	after := map[string][]OperationRef{}
	prev := ""
	for _, ref := range b.diff.base.Operations() {
		key := ref.Method + " " + ref.Path
		if inHead[key] {
			prev = key
			continue
		}
		if b.diff.removed[ref.Operation] {
			after[prev] = append(after[prev], ref)
		}
	}

	out := append([]OperationRef(nil), after[""]...)
	for _, ref := range refs {
		out = append(out, ref)
		out = append(out, after[ref.Method+" "+ref.Path]...)
	}
	return out
}

// operationView builds an operation and its badge. Removed operations are
// built from the base document, without marks.
func (b *builder) operationView(ref OperationRef, tags []string) *opView {
	if b.diff == nil {
		return b.buildOperation(ref, tags)
	}
	od := b.diff.report.Operation(ref.Method, ref.Path)
	if b.diff.removed[ref.Operation] {
		old := *b
		old.d = b.diff.base
		old.diff = nil
		ov := old.buildOperation(ref, tags)
		// The copy lists the type notes it adds on its own slice.
		b.notes = old.notes
		ov.Diff = newBadge(Removed, true)
		ov.Diff.op = od
		return ov
	}

	ov := b.buildOperation(ref, tags)
	if od != nil {
		ov.Diff = newBadge(od.Kind, od.Breaking)
		ov.Diff.op = od
	}
	b.removedParams(ov, ref.Operation)
	b.sampleDiffs(ov, ref, od)
	return ov
}

// removedParams adds a row for every removed parameter to the group of its
// location, creating the group when the location has no parameters left.
func (b *builder) removedParams(ov *opView, op *Operation) {
	removed := b.diff.report.marks.params[op]
	if len(removed) == 0 {
		return
	}
	for _, rp := range removed {
		t := b.oldTree(ForRequest, false)
		t.row(rp.row, 0)
		i := -1
		for j := range ov.ParamGroups {
			if ov.ParamGroups[j].In == rp.in {
				i = j
			}
		}
		if i < 0 {
			ov.ParamGroups = append(ov.ParamGroups, paramGroup{In: rp.in})
			i = len(ov.ParamGroups) - 1
		}
		ov.ParamGroups[i].Rows += template.HTML(t.String())
	}
	sort.SliceStable(ov.ParamGroups, func(i, j int) bool {
		return parameterRank(ov.ParamGroups[i].In) < parameterRank(ov.ParamGroups[j].In)
	})
}

// oldTree writes rows of the base document.
func (b *builder) oldTree(mode ExampleMode, filter bool) *tree {
	return &tree{b: b, d: b.diff.base, mode: mode, filter: filter}
}

// diffRow writes a property or parameter row with its diff marks: removed
// neighbours, the old version of a changed row, then the row itself.
func (t *tree) diffRow(r rowSpec, depth int) {
	if t.b.diff == nil || r.schema == nil {
		t.row(r, depth)
		return
	}
	marks := t.b.diff.report.marks
	for _, old := range marks.before[r.schema] {
		t.oldRow(old, depth)
	}
	m := marks.rows[r.schema]
	if m != nil {
		r.diff = m
		switch {
		case m.added:
			r.mark = markAdd
		case m.old != nil:
			t.oldRow(*m.old, depth)
			r.mark = markAdd
		case m.enum != nil:
			r.mark = markChg
		}
	}
	t.row(r, depth)
	for _, old := range marks.after[r.schema] {
		t.oldRow(old, depth)
	}
}

func (t *tree) oldRow(r rowSpec, depth int) {
	old := &tree{b: t.b, d: t.b.diff.base, mode: t.mode, filter: t.filter}
	old.row(r, depth)
	t.w(old.String())
}

// modelMark returns the mark of a named model's own schema, which carries
// the enum change of a model that is an enum.
func (t *tree) modelMark(name string) *rowMark {
	if t.b.diff == nil || name == "" {
		return nil
	}
	s, ok := t.d.Components.Schemas.Get(name)
	if !ok {
		return nil
	}
	return t.b.diff.report.marks.rows[s]
}

// insideMark flags a row whose nested content changed: a reference to a
// changed model links to its card, an inline object says so in place.
func (t *tree) insideMark(r rowSpec, kidName string) string {
	if t.b.diff == nil || r.mark == markDel {
		return ""
	}
	if kidName != "" {
		md := t.b.diff.report.Model(kidName)
		if md == nil || md.Kind != Changed {
			return ""
		}
		anchor, ok := t.b.models[kidName]
		if !ok {
			return `<span class="mk diff-inside">changed inside</span>`
		}
		return `<a class="mk diff-inside" href="#` + esc(anchor) + `" title="` + esc(kidName) + ` changed">changed inside</a>`
	}
	if r.diff != nil && r.diff.inside {
		return `<span class="mk diff-inside">changed inside</span>`
	}
	return ""
}

// enumChips writes enum values, marking the values added since the base
// and appending the removed ones.
func enumChips(values []any, diff *rowMark) string {
	if diff == nil || diff.enum == nil {
		return chips(values)
	}
	added, removed := enumDelta(diff.enum.was, values)
	isAdded := map[string]bool{}
	for _, v := range added {
		isAdded[enumKey(v)] = true
	}
	var sb strings.Builder
	for _, v := range values {
		if isAdded[enumKey(v)] {
			sb.WriteString(markChip(v, markAdd))
			continue
		}
		sb.WriteString(chips([]any{v}))
	}
	for _, v := range removed {
		sb.WriteString(markChip(v, markDel))
	}
	return sb.String()
}

func markChip(v any, mark string) string {
	return strings.Replace(chips([]any{v}), `class="chip"`, `class="chip `+mark+`"`, 1)
}

func (b *builder) modelBadge(name string) *diffBadge {
	if b.diff == nil {
		return nil
	}
	md := b.diff.report.Model(name)
	if md == nil {
		return nil
	}
	return newBadge(md.Kind, md.Breaking)
}

// changesView is the "What changed" section.
type changesView struct {
	BaseVersion string
	Operations  int
	Models      int
	Breaking    int
	Total       int
	Added       int
	Removed     int
	Changed     int
	Groups      []*changeGroupView
}

type changeGroupView struct {
	Name  string
	Items []*changeItemView
}

// changeItemView is one operation or model in the changelog.
type changeItemView struct {
	Anchor      string
	Method      string
	MethodClass string
	Label       string
	Title       string
	Badge       *diffBadge
	Changes     []changeLineView
}

type changeLineView struct {
	Class    string // diff-add, diff-del or diff-chg
	Where    string // location below the operation or model
	Detail   template.HTML
	Breaking bool
}

func (b *builder) changesView(p *pageView) *changesView {
	if b.diff == nil {
		return nil
	}
	r := b.diff.report
	cv := &changesView{BaseVersion: b.diff.base.Info.Version, Operations: len(r.Operations), Models: len(r.Models), Total: len(r.Changes)}
	for _, c := range r.Changes {
		if c.Breaking {
			cv.Breaking++
		}
	}
	for _, od := range r.Operations {
		switch od.Kind {
		case Added:
			cv.Added++
		case Removed:
			cv.Removed++
		default:
			cv.Changed++
		}
	}

	for _, tv := range p.Groups {
		g := &changeGroupView{Name: tv.Name}
		for _, ov := range tv.Ops {
			if ov.Diff == nil || ov.Diff.op == nil {
				continue
			}
			item := &changeItemView{
				Anchor:      ov.Anchor,
				Method:      ov.Method,
				MethodClass: ov.MethodClass,
				Label:       ov.Path,
				Title:       ov.Title,
				Badge:       ov.Diff,
				Changes:     b.changeLines(ov.Diff.op.Changes),
			}
			g.Items = append(g.Items, item)
		}
		if len(g.Items) > 0 {
			cv.Groups = append(cv.Groups, g)
		}
	}

	// Models that only changed through another model are left out, as in
	// Markdown; their cards still carry the badge.
	models := &changeGroupView{Name: "Models"}
	for _, md := range r.Models {
		var own []Change
		for _, c := range md.Changes {
			if !c.inherited() {
				own = append(own, c)
			}
		}
		if len(own) == 0 {
			continue
		}
		item := &changeItemView{
			Anchor:  b.models[md.Name],
			Label:   md.Name,
			Title:   md.Name,
			Badge:   newBadge(md.Kind, md.Breaking),
			Changes: b.changeLines(own),
		}
		short, ok := b.short[md.Name]
		if ok {
			item.Label = short
		}
		models.Items = append(models.Items, item)
	}
	if len(models.Items) > 0 {
		cv.Groups = append(cv.Groups, models)
	}
	return cv
}

// changeLines lists changes breaking first, each without the operation or
// model it belongs to, with model names linked to their cards.
func (b *builder) changeLines(changes []Change) []changeLineView {
	var out []changeLineView
	for _, c := range changes {
		segs := strings.Split(c.Path, pathSep)
		line := changeLineView{
			Class:    markChg,
			Where:    strings.Join(segs[1:], pathSep),
			Detail:   template.HTML(esc(c.Detail)),
			Breaking: c.Breaking,
		}
		switch c.Kind {
		case Added:
			line.Class = markAdd
		case Removed:
			line.Class = markDel
		}
		if c.inherited() {
			detail := "model " + b.modelLink(c.model) + " changed"
			if len(c.via) > 0 {
				var links []string
				for _, name := range c.via {
					links = append(links, b.modelLink(name))
				}
				detail += " (through " + strings.Join(links, ", ") + ")"
			}
			line.Detail = template.HTML(detail)
		}
		out = append(out, line)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Breaking && !out[j].Breaking
	})
	return out
}

func (b *builder) modelLink(name string) string {
	anchor, ok := b.models[name]
	if !ok {
		return `<code>` + esc(name) + `</code>`
	}
	return `<a class="ref" href="#` + esc(anchor) + `" title="` + esc(name) + `">` + esc(b.short[name]) + `</a>`
}

// Count formats a number with its noun.
func (cv *changesView) Count(n int, one, many string) string {
	return strconv.Itoa(n) + " " + plural(n, one, many)
}

// removedModels lists the component schemas of the base document that head
// no longer has, in base order.
func (st *diffState) removedModels(head *Document) []string {
	var out []string
	for _, name := range st.base.Components.Schemas.Keys {
		_, ok := head.Components.Schemas.Map[name]
		if !ok {
			out = append(out, name)
		}
	}
	return out
}

// removedModelViews builds the cards of the models the head document no
// longer has, from the base document, so their changes have a place.
func (b *builder) removedModelViews() []*modelView {
	if b.diff == nil {
		return nil
	}
	old := *b
	old.d = b.diff.base
	old.diff = nil
	var out []*modelView
	for _, name := range b.diff.removedModels(b.d) {
		s := b.diff.base.Components.Schemas.Map[name]
		if s == nil {
			continue
		}
		short := b.short[name]
		old.addModelSample(name)
		mv := &modelView{
			Name:        name,
			Short:       short,
			Anchor:      b.models[name],
			Table:       b.diff.base.Table(name),
			Description: markdown(s.Description),
			Tree:        old.newTree(ForResponse, false).root(s, name),
			Search:      strings.ToLower(short + " " + name),
			Diff:        b.modelBadge(name),
		}
		_, mv.Inspector = b.samples[mv.Anchor]
		out = append(out, mv)
	}
	// The copy lists the type notes it adds on its own slice.
	b.notes = old.notes
	return out
}

// modelDiffs is what the Changes view of the models needs to set each
// changed, added or removed model's base version beside its head version:
// the two version labels and, by model anchor, a modelDiffView.
type modelDiffs struct {
	Base   string                    `json:"base"`
	Head   string                    `json:"head"`
	Models map[string]*modelDiffView `json:"models"`
}

// modelDiffView is one model of modelDiffs: its change kind (added, removed
// or changed), its base sample, absent when it was added (the head sample
// is in the model samples), and its changes.
type modelDiffView struct {
	Kind    string            `json:"k"`
	Base    *modelSample      `json:"b,omitempty"`
	Changes []modelChangeView `json:"c,omitempty"`
}

// modelChangeView is one change of a model: Path is the property it is at
// below the model, segments joined by " › " ("" for the model itself),
// Kind added, removed, changed or inside (a model it holds changed, named
// by Model, the anchor of its card, and Detail, its display name).
type modelChangeView struct {
	Path     string `json:"p"`
	Kind     string `json:"k"`
	Detail   string `json:"d"`
	Breaking bool   `json:"x,omitempty"`
	Model    string `json:"m,omitempty"`
}

func (b *builder) modelDiffsJSON() template.JS {
	if b.diff == nil {
		return ""
	}
	out := modelDiffs{Base: b.diff.base.Info.Version, Head: b.d.Info.Version, Models: map[string]*modelDiffView{}}
	for _, md := range b.diff.report.Models {
		anchor, ok := b.models[md.Name]
		if !ok {
			continue
		}
		mv := &modelDiffView{Kind: md.Kind.String()}
		if md.Kind != Added {
			mv.Base = b.baseSample(md.Name)
		}
		for _, c := range md.Changes {
			segs := strings.Split(c.Path, pathSep)
			mv.Changes = append(mv.Changes, b.changeView(c, strings.Join(segs[1:], pathSep)))
		}
		out.Models[anchor] = mv
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return template.JS(raw)
}

// changeView is the change c at path, relative to the model or sample it
// belongs to.
func (b *builder) changeView(c Change, path string) modelChangeView {
	cv := modelChangeView{Path: path, Kind: c.Kind.String(), Detail: c.Detail, Breaking: c.Breaking}
	if c.inherited() {
		cv.Kind = "inside"
		cv.Model = b.models[c.model]
		cv.Detail = b.display(c.model)
	}
	return cv
}

// baseSample is the dialog sample of the component schema name in the base
// document, or nil when it has none.
func (b *builder) baseSample(name string) *modelSample {
	v, notes := b.diff.base.modelExample(&Schema{Ref: refSchemas + name}, b.display)
	if len(notes) == 0 {
		return nil
	}
	var raw bytes.Buffer
	appendJSON(&raw, v)
	return &modelSample{Value: raw.Bytes(), Types: b.noteRef(strings.Join(notes, "\n"))}
}

// sampleDiffView is the diff of one response sample: Kind added, removed or
// changed; Base the base sample of a changed one (a removed response's
// panel shows the base sample itself); Model the anchor of the model the
// sample shows, whose changes, and those of the models inside it, the client
// adds by path; Changes the operation's own changes below the response,
// with paths relative to it.
type sampleDiffView struct {
	Kind    string            `json:"k"`
	Base    *modelSample      `json:"b,omitempty"`
	Model   string            `json:"m,omitempty"`
	Changes []modelChangeView `json:"c,omitempty"`
}

// sampleDiffs gives each response sample of a changed operation that
// changed its diff and badge, and adds a sample built from the base
// document for every response the head no longer has.
func (b *builder) sampleDiffs(ov *opView, ref OperationRef, od *OperationDiff) {
	if od == nil || od.Kind != Changed {
		return
	}
	bref, ok := b.diff.baseOperation(ref.Method, ref.Path)
	if !ok {
		return
	}
	for i := range ov.ResSamples {
		s := &ov.ResSamples[i]
		sd := &sampleDiffView{Kind: "changed", Changes: b.responseChanges(od, s.Label, s.Type)}
		br, ok := bref.Operation.Responses.Get(s.Label)
		if !ok {
			sd.Kind = "added"
		} else {
			sd.Base = b.baseMediaSample(b.diff.base.Response(br), s.Type)
		}
		if sd.Kind == "changed" && len(sd.Changes) == 0 {
			continue
		}
		if s.Model != nil {
			sd.Model = s.Model.Anchor
		}
		kind := Changed
		if sd.Kind == "added" {
			kind = Added
		}
		s.Diff = newBadge(kind, anyBreaking(sd.Changes))
		s.DiffRef = b.diff.addSample(sd)
	}

	old := *b
	old.d = b.diff.base
	old.diff = nil
	for _, code := range bref.Operation.Responses.Keys {
		_, ok := ref.Operation.Responses.Get(code)
		if ok {
			continue
		}
		r := b.diff.base.Response(bref.Operation.Responses.Map[code])
		if r == nil {
			continue
		}
		for _, ct := range r.Content.Keys {
			mt := r.Content.Map[ct]
			if mt == nil {
				continue
			}
			example := b.exampleJSON(b.diff.base, mt, ForResponse)
			if example == "" {
				continue
			}
			sv := sampleView{Label: code, Class: statusClass(code), Type: ct, Model: old.sampleModel(mt.Schema), Example: textHTML(example), Removed: true, Diff: newBadge(Removed, false)}
			notes := b.diff.base.mediaTypes(mt, ForResponse, b.display)
			if notes != "" {
				sv.Types = strconv.Itoa(b.noteRef(notes))
			}
			sd := &sampleDiffView{Kind: "removed", Changes: b.responseChanges(od, code, ct)}
			if sv.Model != nil {
				sd.Model = sv.Model.Anchor
			}
			sv.DiffRef = b.diff.addSample(sd)
			ov.ResSamples = append(ov.ResSamples, sv)
			break
		}
	}
	sort.SliceStable(ov.ResSamples, func(i, j int) bool {
		return statusRank(ov.ResSamples[i].Label) < statusRank(ov.ResSamples[j].Label)
	})
}

// responseChanges are the changes of od below its response code, with paths
// relative to the response's content of type ct.
func (b *builder) responseChanges(od *OperationDiff, code, ct string) []modelChangeView {
	prefix := od.Key() + pathSep + "response " + code
	var out []modelChangeView
	for _, c := range od.Changes {
		rel := ""
		switch {
		case c.Path == prefix:
		case strings.HasPrefix(c.Path, prefix+pathSep):
			rel = c.Path[len(prefix+pathSep):]
		default:
			continue
		}
		// A response with several content types puts the type first.
		if rel == ct {
			rel = ""
		}
		rel = strings.TrimPrefix(rel, ct+pathSep)
		out = append(out, b.changeView(c, rel))
	}
	return out
}

// baseMediaSample is the sample of the base response r for content type ct,
// or for its only content type, or nil.
func (b *builder) baseMediaSample(r *Response, ct string) *modelSample {
	if r == nil {
		return nil
	}
	mt, ok := r.Content.Get(ct)
	if !ok && r.Content.Len() == 1 {
		mt = r.Content.Map[r.Content.Keys[0]]
	}
	if mt == nil {
		return nil
	}
	example := b.exampleJSON(b.diff.base, mt, ForResponse)
	if example == "" {
		return nil
	}
	sample := &modelSample{Value: json.RawMessage(example), Types: -1}
	notes := b.diff.base.mediaTypes(mt, ForResponse, b.display)
	if notes != "" {
		sample.Types = b.noteRef(notes)
	}
	return sample
}

func anyBreaking(changes []modelChangeView) bool {
	for _, c := range changes {
		if c.Breaking {
			return true
		}
	}
	return false
}

// baseOperation finds the base document's operation at method and path.
func (st *diffState) baseOperation(method, path string) (OperationRef, bool) {
	for _, ref := range st.base.Operations() {
		if ref.Method == method && ref.Path == path {
			return ref, true
		}
	}
	return OperationRef{}, false
}

// addSample lists a sample diff and returns its index.
func (st *diffState) addSample(sd *sampleDiffView) string {
	st.samples = append(st.samples, sd)
	return strconv.Itoa(len(st.samples) - 1)
}

// sampleDiffsJSON is the sample diffs the response panels point at.
// Marshal escapes <, > and &, so the JSON is safe inside a script element.
func (b *builder) sampleDiffsJSON() template.JS {
	if b.diff == nil || len(b.diff.samples) == 0 {
		return ""
	}
	raw, err := json.Marshal(b.diff.samples)
	if err != nil {
		return ""
	}
	return template.JS(raw)
}

// machineDiffs marks the ports of the changed operations' machines: an
// input or output that was added, changed or removed gets its badge, and
// the inputs and outputs the head no longer has come back as removed ports.
func (b *builder) machineDiffs(ops []*opView) {
	if b.diff == nil {
		return
	}
	for _, ov := range ops {
		if ov.Diff == nil || ov.Diff.op == nil || ov.Diff.op.Kind != Changed {
			continue
		}
		od := ov.Diff.op
		key := od.Key()
		m := &ov.Machine
		for i := range m.Inputs {
			p := &m.Inputs[i]
			switch {
			case p.Key == "in:body":
				p.Diff = portBadge(od, key+pathSep+"request body")
			case p.Key == "in:optional":
				var badges []*diffBadge
				for _, name := range p.Names {
					for _, loc := range []string{"query", "header", "cookie", "path"} {
						badges = append(badges, portBadge(od, key+pathSep+loc+pathSep+name))
					}
				}
				p.Diff = mergeBadges(badges)
			case strings.HasPrefix(p.Key, "in:"):
				loc, name, _ := strings.Cut(strings.TrimPrefix(p.Key, "in:"), ":")
				p.Diff = portBadge(od, key+pathSep+loc+pathSep+name)
			}
		}
		for i := range m.Outputs {
			p := &m.Outputs[i]
			p.Diff = portBadge(od, key+pathSep+"response "+p.Name)
		}

		removed := newBadge(Removed, false)
		for _, rp := range b.diff.report.marks.params[ov.ref.Operation] {
			p := portView{Key: "in:removed:" + rp.in + ":" + rp.row.name, Name: rp.row.name + " (" + rp.in + ")", Kind: "any", Ghost: true, Diff: removed}
			m.Inputs = append(m.Inputs, p)
		}
		bref, ok := b.diff.baseOperation(ov.Method, ov.Path)
		if !ok {
			continue
		}
		old := *b
		old.d = b.diff.base
		for _, code := range bref.Operation.Responses.Keys {
			_, ok := ov.ref.Operation.Responses.Get(code)
			if ok {
				continue
			}
			name := code
			s := old.responseSchema(bref.Operation.Responses.Map[code])
			if s != nil {
				name += " " + old.port(s, ForResponse, 0).Type
			}
			m.Outputs = append(m.Outputs, portView{Key: "out:removed:" + code, Name: name, Kind: "any", Ghost: true, Diff: removed})
		}
	}
}

// portBadge is the badge of the port whose changes sit at path: added when
// the port itself was, changed when anything at or below it changed.
func portBadge(od *OperationDiff, path string) *diffBadge {
	var badge *diffBadge
	for _, c := range od.Changes {
		if c.Path != path && !strings.HasPrefix(c.Path, path+pathSep) {
			continue
		}
		kind := Changed
		if c.Path == path && c.Kind == Added && strings.HasSuffix(c.Detail, "added") && !strings.HasPrefix(c.Detail, "content type") {
			kind = Added
		}
		if badge == nil {
			badge = newBadge(kind, c.Breaking)
			continue
		}
		if kind != Added && badge.Letter == "A" {
			continue
		}
		badge.Breaking = badge.Breaking || c.Breaking
	}
	return badge
}

// mergeBadges is one changed badge for several, nil when all are.
func mergeBadges(badges []*diffBadge) *diffBadge {
	var out *diffBadge
	for _, b := range badges {
		if b == nil {
			continue
		}
		if out == nil {
			out = newBadge(Changed, false)
		}
		out.Breaking = out.Breaking || b.Breaking
	}
	return out
}
