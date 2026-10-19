package reference

import (
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

	for _, tv := range p.Tags {
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
