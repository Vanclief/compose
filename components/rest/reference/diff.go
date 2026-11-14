package reference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
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
	// via the models with their own changes it changed through, the first
	// maxVia of them, with more counting the others.
	model string
	via   []string
	more  int
	// parts is Detail, when it names models, as text and model names in
	// turn, text first, so Markdown can write exactly those names as code.
	parts []string
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
	path      *diffPath
	detail    string
	breakReq  bool
	breakResp bool
	// strictReq and strictResp are the flags the change has when judged
	// strictly, see walk.addStrict, read where the schema it was found in is
	// referred to under a not.
	strictReq  bool
	strictResp bool
	ctx        diffCtx
	parts      []string // see Change
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

// breakingAt reports whether c, a change of a model, breaks ctx at a use of
// the model under nots: judged strictly, see walk.addStrict, when strict is
// set, for at least one not, and breaking the other context when not is
// set, for an odd number, as there what the model accepts the use refuses.
func (c rawChange) breakingAt(ctx diffCtx, not, strict bool) bool {
	req := c.breakReq
	resp := c.breakResp
	if strict {
		req = c.strictReq
		resp = c.strictResp
	}
	if not {
		ctx = other(ctx)
	}
	switch ctx {
	case ctxRequest:
		return req
	case ctxResponse:
		return resp
	}
	return req || resp
}

// refUse is a $ref to the same named model on both sides. The model is
// compared once on its own; the reference only inherits its result.
type refUse struct {
	name string
	path *diffPath
	ctx  diffCtx
	// not is set for a use under an odd number of nots, where what the
	// model accepts the use refuses, so the model's changes break the other
	// way, and strict for a use under at least one, where they are judged
	// strictly, see rawChange.breakingAt.
	not    bool
	strict bool
	// quiet is set for a use that only tells the model the context it is
	// read in, so its changes are judged there: a model compared in place,
	// whose changes the comparison reported itself, or a component that is
	// another name for the model the use names. It gives no entry of its own
	// and does not make the walk's model refer to it, see markUsage.
	quiet bool
}

// useKey is a context a model is used in, and how that use stands under
// nots, see refUse.
type useKey struct {
	ctx    diffCtx
	not    bool
	strict bool
}

// other is the context whose breaking changes break ctx under a not: a
// change that breaks a request there breaks a response at the use, and the
// reverse.
func other(ctx diffCtx) diffCtx {
	switch ctx {
	case ctxRequest:
		return ctxResponse
	case ctxResponse:
		return ctxRequest
	}
	return ctx
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
	// required, description, flags or validation keywords changed; it is
	// written before the row.
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
	flat       map[flatKey]*Schema
	keys       map[*Schema]string
	// users lists, per model, the models that refer to it, built on first
	// use; deep holds the models that changed, themselves or through
	// references, and breakSets, per context, the models with a change
	// that breaks it, see breaks.
	users     map[string][]modelUse
	deep      map[string]bool
	breakSets map[diffCtx]map[modelUse]bool
	// scc, sccIndex, sccLow and sccStack hold sources' walk of the models'
	// strongly connected components, and srcs the sorted models with
	// changes of their own each component reaches.
	scc      map[string]int
	sccIndex map[string]int
	sccLow   map[string]int
	sccStack []string
	srcs     [][]string
	// texts holds the parts of each "model M changed" entry, per model and
	// whether it is under a not, see useChanges.
	texts map[modelUse]useText
	// unfolded holds the folded unions given back their one variant, see
	// unfold.
	unfolded map[unfoldKey]*Schema
}

// modelUse is a model and how it is referred to under nots, see refUse.
type modelUse struct {
	name   string
	not    bool
	strict bool
}

// useText is the text of a "model M changed" entry, see useChanges, kept
// per model and not so that the entries of one model share it.
type useText struct {
	parts  []string
	detail string
	via    []string
	more   int
}

// maxVia is how many models a "model M changed (through ...)" entry names;
// it counts the others, so a model that reaches thousands of changed models
// does not write all their names at every use of it.
const maxVia = 50

// flatKey is a schema flattened against a document.
type flatKey struct {
	doc *Document
	s   *Schema
}

// flatten is flattenSchema with siblings set, kept per document and schema,
// on which alone the result depends, so a model used in many places is
// flattened once rather than at each use. Treat the result as read-only.
func (d *differ) flatten(doc *Document, s *Schema) *Schema {
	key := flatKey{doc: doc, s: s}
	f, ok := d.flat[key]
	if !ok {
		f = flattenSchema(doc, s, true)
		d.flat[key] = f
	}
	return f
}

// walk collects the changes of one operation or model.
type walk struct {
	d       *differ
	ctx     diffCtx
	changes []rawChange
	uses    []refUse
	// quiet holds, per context, the models given a quiet use, see refUse,
	// and the schemas whose merged models were, so each is done once.
	quiet map[quietKey]bool
	// pairs guards structural comparisons across differently named models
	// against cycles.
	pairs map[[2]*Schema]bool
}

// diffPath is where a comparison stands: its last segment, linked to the
// path above it, so a step deeper shares the rest and costs the same at
// any depth. It is written out, see String, only where a change or a use
// is reported. items marks a step into the items of an array, see
// itemsPath.
type diffPath struct {
	up    *diffPath
	seg   string
	items bool
}

// rootPath is the path of one segment, an operation or a model.
func rootPath(seg string) *diffPath {
	return &diffPath{seg: seg}
}

// String writes the segments of p joined by pathSep, a step into items as
// [] after a segment without a space, tags becoming tags[], and as a
// segment items after one with a space or at the start: response 200 ›
// items.
func (p *diffPath) String() string {
	var steps []*diffPath
	for q := p; q != nil; q = q.up {
		steps = append(steps, q)
	}
	var sb strings.Builder
	started := false
	open := false
	for i := len(steps) - 1; i >= 0; i-- {
		q := steps[i]
		if q.items && open {
			sb.WriteString("[]")
			continue
		}
		if started {
			sb.WriteString(pathSep)
		}
		started = true
		seg := q.seg
		if q.items {
			seg = "items"
		}
		sb.WriteString(seg)
		open = !strings.Contains(seg, " ")
	}
	return sb.String()
}

// Diff compares two documents structurally: operations by method and path,
// schemas property by property after allOf flattening and ref resolution.
// Flattening keeps the tightest of the bounds allOf members set and the
// types they all allow, merges a property, items or additional properties
// that several members define, see Document.Flatten, and the keywords
// written beside a $ref apply together with the model it names, on a schema
// and on each allOf member alike.
//
// Where both sides refer to the same model, under any spelling of the
// reference, as a $ref or as single-member allOf wrappers nested around one,
// with only keywords beside the reference and on each wrapper that give it
// no structure of its own, that is none of properties, required, items,
// enum, oneOf, anyOf, not, discriminator and additionalProperties, the model
// is not expanded: what applies there, the keywords with the model, is
// compared, the model itself once on its own, and every operation or model
// that reaches a changed model through references is reported as changed.
// The use reports a change only when its own keywords change what applies
// there, compared with the model as the head document has it, so a change to
// the model alone is reported in the model only; it then reports what
// applied there before, its keywords with the model as the base document has
// it, against what applies after. Any other shape is compared in full, such
// as an allOf of a reference and other members, a reference or a wrapper
// with structure beside it, or a base reference that leads in the head
// document to another model than the head side does, and a change inside the
// model then shows both there and in the model. Two sides whose references
// in those shapes name the same component, though, refer to that component
// as the model, also when on one side it only refers to another model, as a
// component with nothing but a reference does until it gets a keyword of its
// own, which is then reported once, in that component. A type on one side
// only, where both sides have oneOf or anyOf variants, as beside a reference
// to a model that is a oneOf and states no type, is no change when it
// restates the variants, whether the model is expanded or not: every variant
// states a type, each of the variant's non-null types is one the type names,
// an integer counting as a number, and no variant lets null through that the
// type refuses. Otherwise, as beside variants of string and integer, beside
// nullable variants, or beside a reference to a model that states no type
// and has no variants, it is compared like any type.
//
// An allOf of two or more members is nullable when the schema itself says
// so, or when a member does and every member lets null through, as one that
// is nullable or that states none of type, enum, const, oneOf, anyOf and not
// does; a single member, as in the 3.0 spelling of a nullable reference,
// passes its nullable on.
//
// These limits follow. enum, pattern, multipleOf, const and not, and the
// oneOf and anyOf lists, like default and the other keywords that cannot be
// combined across allOf members, are compared as the first one found, the
// schema's own and then its members' in order, so the list of a second
// member is not compared, and, of a property two members define, the later
// definition's. Each keyword is compared on its own, so a change
// another keyword makes moot is still reported with its own flag: a
// maxLength changed under an enum whose values all meet both, bounds that
// admit no value changed for others that admit none either, a const added
// under an enum of that one value, a minProperties of 1 added where a
// required property was dropped. A schema with nullable and an enum or const
// without null is read as letting null through, so nullable removed there is
// breaking in a request though neither side accepts null, and a oneOf in
// which two variants let null through is read as letting null through,
// though the oneOf then refuses it. A schema with a not is read as refusing
// null, so nullable removed beside a not breaks a request, and given beside
// one breaks a response, whatever the not refuses. A type of only null is
// read as the type null, made nullable, so {type: string} or {} to {type:
// null} reads type changed to null, which narrows, beside made nullable, and
// the reverse widens, as does {type: null} to {type: [string, null]}, and a
// oneOf or anyOf of only {type: null} reads as {type: null} does. Given by an
// allOf member, as in {allOf: [{type: null}]}, or {allOf: [{$ref: N}]} with N
// {type: null}, it is read as nullable with no type, so {type: string} to
// that reads type changed from string to any, which widens, though only null
// is then accepted, and {type: null} to that reads type changed from null to
// any, breaking a response, though the two say the same. Beside a $ref it is
// not read as a type either, so {$ref: S} to {$ref: S, type: null}, with S
// {type: string}, reads made nullable only, which breaks a response only,
// though nothing is accepted then. Each keyword is compared on its own, so
// {enum: [null]} or {const: null} to {type: null} reads type changed from any
// to null, breaking a request, though each accepts only null, and variants
// given beside type: null narrow, breaking a request only. A {type: null}
// variant is folded into nullable and is not seen as a type beside the type
// of its parent, and an enum of only null is read as an enum, not as a type
// of null. A not is
// compared as written, see below, so any change inside it is reported once,
// as not changed, and a model either side names under an odd number of
// nots, those inside variants counted, that changed is reported where the
// outermost not stands, or, for a not in a variant of a list whose variants
// changed otherwise, where that list stands, as not changed: model M
// changed, its changes judged strictly and with their flags swapped; under
// an even number it reads model M changed, its changes still judged
// strictly, with their flags as they are. Judged strictly, enum values added
// and additional properties allowed again widen the model, and a format
// added narrows it, one dropped widens it and one changed does both. An
// optional property added is not judged strictly, though it narrows a model
// that allows other properties, so under a not in a response it breaks
// nothing: M: {type: object, properties: {a}} gaining b: {type: integer}
// refuses {"b": "s"}, which {not: {$ref: M}} then accepts. A type
// is read as object from properties and additionalProperties and as array
// from items where none is stated, so a stated type removed beside them is
// not reported, though the schema then accepts any value; only whether a
// nullable change is breaking reads no type there. patternProperties is not
// read, so a property removed from an object that allows no other
// properties breaks a request even when a pattern still accepts it.
// Variants added to a oneOf are read as widening, as for an anyOf, though a
// value that newly matches two of them is refused, a variant listed more or
// fewer times in a oneOf, which changes the values that match exactly one,
// breaks nothing, and a list of the same length is compared by position, so
// variants that were only reordered are reported as changed where they now
// stand. A comparison that walks references in place, as one of models
// renamed between the documents, or one through an allOf that joins a model
// to other members or a reference with structure beside it, which are
// compared in place even where the documents are the same, can take long: a
// chain of renamed models in which each refers twice to the next is compared
// once for every path through it, twice as many with each model; a model
// whose properties each join it to a bound through an allOf is compared
// once for every order of those properties, as the walk's cycle guard only
// stops a schema met again on the same path, so ten such properties take
// about ten seconds and eleven two minutes, and where each joins it to a not
// of itself instead, as allOf: [{$ref: M}, {not: {$ref: M}}], the models the
// nots name are recorded as used at every such path, so memory grows as fast
// as time: nine take 2 seconds and 0.4 GB, ten 20 seconds and 4.5 GB; a chain
// of models in which each refers to the next from a property, with structure
// beside the reference, such as properties, is compared at each model for the
// rest of the chain, n²/2 schemas for n models, about a second for 2,000, and
// where what is beside the references changed, the change is reported at
// every level of every model above it, 2,001,001 changes for 2,000 models,
// which takes 0.3 seconds and 0.5 GB for 500 models, 2.3 seconds and 3.4 GB
// for 1,000, and 32 seconds and 24 GB for 2,000; and a chain of models each
// of which is itself a reference to the next with structure beside it, M0:
// {$ref: M1, properties: {...}}, takes time cubic in its length, as each
// model is flattened to the end of the chain, whether or not the documents
// are the same: seconds for 500 models, tens of seconds for 1,000 and minutes
// for 2,000, where the same chain written as allOf: [{$ref: M1}, {properties:
// {...}}] took about as long before too, 2 to 3 seconds for 500 models. A
// model M changed (through ...) entry names the first 50 of the
// models it changed through and counts the others. A model that no operation
// reaches, or that only something the head document added refers to, such as
// a new operation, property or variant, has no recorded use in a request or a
// response, so in the Models section, and in the count of breaking changes,
// each of its changes is breaking when it would break either. A model is used
// where an operation reaches it also where it is compared in place, its
// changes then listed there too, as for a property that switched to it from a
// component that is gone, or one that joins it to other members by an allOf
// or has structure beside its reference, and so is a component that only
// refers to another model, as X: {$ref: M} or {allOf: [{$ref: M}]} does,
// where a reference to it is used, also where one switched to it, or from it,
// which uses it in the context of the switch, though the use is reported
// under the model it names. An operation that uses a model reports model M
// changed with the flag the model's change has in its context, also
// when keywords at the use mask that change there, as a maxLength of 3 beside
// a reference does a model's maxLength lowered from 10 to 5.
//
// Breaking changes follow oasdiff. A schema change that narrows the values
// accepted breaks a request, where a client sending what used to be valid is
// now refused, and not a response; one that widens them breaks a response,
// where a client may receive what it was never told to expect, and not a
// request. Narrowing: a raised minimum or lowered maximum (an exclusive
// bound is tighter than an inclusive one at the same value, and on a schema
// of type integer each bound is first taken as the inclusive bound that
// admits the same integers, so minimum: 1 and exclusiveMinimum: 0 are the
// same); a raised minLength, minItems or minProperties; a lowered maxLength,
// maxItems or maxProperties; any of these bounds added, except a minLength,
// minItems or minProperties of 0, which every value meets; a pattern,
// multipleOf or const added; uniqueItems set; a type stated where there was
// none; an enum where there was none, or enum values removed; nullable
// removed from a schema that then refuses null; a not added;
// additionalProperties: false, or the false schema written as {not: {}},
// with nothing but annotations beside the not or under it, on an object that
// allowed other properties; items: false, or {not: {}}, on an array that
// allowed items. Widening: the reverse bound changes, any bound removed,
// again except a minLength, minItems or minProperties of 0; a pattern,
// multipleOf or const removed; uniqueItems unset; a type dropped; an enum
// dropped; nullable added to a schema that refused null; a not dropped;
// items allowed again. Between two nots, the schemas under them are compared
// as written, a $ref the same as another where it names the same component,
// whose model is not compared there, properties, required names, types and
// enum values in any order, and extensions left out: written the same, they
// report nothing; different only in annotations, title,
// description, default, example, examples, deprecated, readOnly and
// writeOnly, not changed breaks neither; different otherwise, not changed
// breaks both, as which way a change under a not moves is not worked out,
// also where only a reference was renamed to a model written the same.
// Otherwise, a change from one stated type to another, but for a type of
// only null, see above, and a pattern, multipleOf or const replaced by a
// different one, break both; enum values
// added to an enum, additional properties allowed again, as a new response
// property is, a format change and a default changed, added or removed break
// neither, and so does a nullable change on a schema that lets null through
// on both sides, such as one that states no type. When the type changes between integer and number,
// a bound's detail gives the bound the integer side admitted beside the one
// written, as in minimum lowered from 0.5 (1 for integers) to 0.7. The
// schemas of the additional properties of two objects that allow them are
// compared like any other schemas, under <key>, with the empty schema, which
// allows any value, standing for additionalProperties: true, {} or none at
// all, so no change between those three is reported; the items of two
// schemas of the same type are compared the same way, with the empty schema
// standing for items absent.
//
// oneOf and anyOf variants are compared by position where both sides have
// lists of the same keyword and length, each keyword on its own. Any other
// change of variants is judged from the variants as written, without
// comparing them, a $ref the same as another when it names the same
// component, properties, required names, types and enum values in any order,
// and annotations left out, and breaks both, as which way it moved
// cannot be told, except in these cases. Variants only added to a list
// widen, and only removed narrow, and the same variants with one listed more
// or fewer times break neither. Variants given where there were none, with
// nothing beside them but annotations and nullable, in place of a schema
// written as one of them, widen, and dropped back to such a schema narrow;
// a change of nullable is reported on its own. Variants given beside other
// keywords narrow, as a value must then match one of them too, and dropped
// from beside them widen. anyOf turned into oneOf with the same variants
// narrows, and the reverse widens. The models that the variants kept,
// written the same on both sides, refer to are recorded as used there, so a
// change to one reaches the use; a variant added or removed is judged by the
// change of variants alone, as no client sent or received it there before,
// or will after. The keywords beside the variants are compared on their
// own, with their own flags. A 3.1 union of one schema and {type: null},
// which Parse folds into that schema made nullable, is compared as the list
// of that one variant, under the keyword of the other side's variants, where
// the other side lists variants and the one schema has none of its own once
// flattened, so a variant added beside it reads as added, as in the 3.0
// spelling, and oneOf: [S, I] to anyOf: [S, {type: null}] reads one of
// variants removed. Where the one schema has variants of its own, the two are
// compared as they are, as before, so anyOf: [{$ref: U}, {type: null}], with
// U a oneOf, to the same list with a variant added reads one of variants
// changed to any of, breaking both, though it only widens.
//
// The other breaking changes: a removed operation; a removed (or renamed)
// response property, or a property removed from an object that allows no
// other properties, or whose additionalProperties schema limits values and
// is not written as the property's schema was, annotations left out and a
// $ref the same as another where it names the same component, which breaks
// a request that still sends it too, also where that schema accepts every
// value the property's did, as the two are not compared, while where it is
// written so and the object allowed no other properties before, the models
// that schema names are used there, so their changes reach the request; a
// removed response header, or one no longer required; a response property no
// longer required, which a response may now leave out; a new required
// request property, parameter or request body; an existing request property,
// parameter or request body made required, also a name required where no
// property of that name is declared. A response header named Content-Type is
// not compared, as OpenAPI says its definition is ignored. A discriminator
// given where there was none breaks a request, one dropped a response, and
// its propertyName changed both; of its mapping, compared as written, a
// value added breaks neither, one removed breaks a request and one that
// names another component both. A parameter's style or explode changed,
// each with the default of its location where unset, breaks a request,
// except explode where both sides state a type that is neither an array nor
// an object. Everything else is a non-breaking change.
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
		flat:      map[flatKey]*Schema{},
		keys:      map[*Schema]string{},
		breakSets: map[diffCtx]map[modelUse]bool{},
		scc:       map[string]int{},
		sccIndex:  map[string]int{},
		sccLow:    map[string]int{},
		texts:     map[modelUse]useText{},
		unfolded:  map[unfoldKey]*Schema{},
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
		res := w.schema(bs, hs, rootPath(name), true)
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

	usage := map[string]map[useKey]bool{}
	for _, od := range d.operations() {
		for _, u := range od.uses {
			d.markUsage(u.name, useKey{ctx: u.ctx, not: u.not, strict: u.strict}, usage)
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

// markUsage records that name is read as key says, directly or through
// models that reference it. A use already recorded was followed when it was
// recorded, so each model is followed once per key.
func (d *differ) markUsage(name string, key useKey, usage map[string]map[useKey]bool) {
	if usage[name][key] {
		return
	}
	if usage[name] == nil {
		usage[name] = map[useKey]bool{}
	}
	usage[name][key] = true
	st := d.models[name]
	if st == nil {
		return
	}
	for _, u := range st.uses {
		d.markUsage(u.name, useKey{ctx: key.ctx, not: key.not != u.not, strict: key.strict || u.strict}, usage)
	}
}

// referrers returns, per model, the models that refer to it, each with how
// it does under nots.
func (d *differ) referrers() map[string][]modelUse {
	if d.users != nil {
		return d.users
	}
	d.users = map[string][]modelUse{}
	for name, st := range d.models {
		for _, u := range st.uses {
			if u.quiet {
				continue
			}
			d.users[u.name] = append(d.users[u.name], modelUse{name: name, not: u.not, strict: u.strict})
		}
	}
	return d.users
}

// changed reports whether model name changed itself or through a model it
// references. The models that did are found once, from the models with
// changes of their own to the models that refer to them.
func (d *differ) changed(name string) bool {
	if d.deep == nil {
		d.deep = map[string]bool{}
		var queue []string
		for n, st := range d.models {
			if len(st.changes) > 0 {
				d.deep[n] = true
				queue = append(queue, n)
			}
		}
		users := d.referrers()
		for len(queue) > 0 {
			n := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			for _, u := range users[n] {
				if !d.deep[u.name] {
					d.deep[u.name] = true
					queue = append(queue, u.name)
				}
			}
		}
	}
	return d.deep[name]
}

// breaks reports whether model name has a change, itself or through
// references, that breaks ctx at a use under nots as not and strict say,
// see rawChange.breakingAt. The models that break ctx are found once per
// context, from the models whose own changes break it to the models that
// refer to them: a model used as m says reaches a model it refers to with
// the not of the reference added to m's, an odd number of nots becoming
// even and the reverse, and judged strictly when m is or the reference is
// under a not.
func (d *differ) breaks(name string, ctx diffCtx, not, strict bool) bool {
	set, ok := d.breakSets[ctx]
	if !ok {
		set = map[modelUse]bool{}
		var queue []modelUse
		for n, st := range d.models {
			for _, use := range []modelUse{{name: n}, {name: n, not: true}, {name: n, strict: true}, {name: n, not: true, strict: true}} {
				for _, c := range st.changes {
					if c.breakingAt(ctx, use.not, use.strict) {
						set[use] = true
						queue = append(queue, use)
						break
					}
				}
			}
		}
		users := d.referrers()
		for len(queue) > 0 {
			m := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			for _, u := range users[m.name] {
				if u.strict && !m.strict {
					continue
				}
				nexts := []modelUse{{name: u.name, not: m.not != u.not, strict: m.strict}}
				if u.strict {
					nexts = append(nexts, modelUse{name: u.name, not: m.not != u.not})
				}
				for _, next := range nexts {
					if !set[next] {
						set[next] = true
						queue = append(queue, next)
					}
				}
			}
		}
		d.breakSets[ctx] = set
	}
	return set[modelUse{name: name, not: not, strict: strict}]
}

// sources lists, sorted, the models with changes of their own that name
// reaches through references. Models that reach each other reach the same
// models, so the list is found once per strongly connected component of
// the references, from the lists of the components it refers to.
func (d *differ) sources(name string) []string {
	_, ok := d.scc[name]
	if !ok {
		d.connect(name)
	}
	return d.srcs[d.scc[name]]
}

// connect is Tarjan's step for name in sources' walk: it numbers name,
// follows its references, and when name is the root of a component, pops
// the component and records what it reaches.
func (d *differ) connect(name string) {
	index := len(d.sccIndex)
	d.sccIndex[name] = index
	d.sccLow[name] = index
	d.sccStack = append(d.sccStack, name)
	st := d.models[name]
	if st != nil {
		for _, u := range st.uses {
			_, seen := d.sccIndex[u.name]
			_, done := d.scc[u.name]
			switch {
			case u.quiet:
			case !seen:
				d.connect(u.name)
				d.sccLow[name] = min(d.sccLow[name], d.sccLow[u.name])
			case !done:
				d.sccLow[name] = min(d.sccLow[name], d.sccIndex[u.name])
			}
		}
	}
	if d.sccLow[name] != index {
		return
	}
	id := len(d.srcs)
	var members []string
	for {
		top := d.sccStack[len(d.sccStack)-1]
		d.sccStack = d.sccStack[:len(d.sccStack)-1]
		d.scc[top] = id
		members = append(members, top)
		if top == name {
			break
		}
	}
	found := map[string]bool{}
	for _, m := range members {
		ms := d.models[m]
		if ms == nil {
			continue
		}
		if len(ms.changes) > 0 {
			found[m] = true
		}
		for _, u := range ms.uses {
			c := d.scc[u.name]
			if u.quiet || c == id {
				continue
			}
			for _, s := range d.srcs[c] {
				found[s] = true
			}
		}
	}
	var out []string
	for s := range found {
		out = append(out, s)
	}
	sort.Strings(out)
	d.srcs = append(d.srcs, out)
}

// useChanges turns the references of a walk into "model X changed" entries
// for the models that changed, "not changed: model X changed" for one
// referred to under a not, which breaks the other way, see
// rawChange.breakingAt. Each entry breaks as breaking says of its uses, of
// any of them where several give the same entry.
func (d *differ) useChanges(uses []refUse, breaking func(refUse) bool) []Change {
	var out []Change
	seen := map[string]int{}
	for _, u := range uses {
		if u.quiet || !d.changed(u.name) {
			continue
		}
		path := u.path.String()
		key := path + "\x00" + u.name + "\x00" + strconv.FormatBool(u.not)
		i, ok := seen[key]
		if ok {
			// The same entry from another use, which may be judged
			// strictly where this one is not: it breaks where either does.
			out[i].Breaking = out[i].Breaking || breaking(u)
			continue
		}
		seen[key] = len(out)
		text := d.useText(u)
		out = append(out, Change{
			Kind:     Changed,
			Path:     path,
			Detail:   text.detail,
			Breaking: breaking(u),
			model:    u.name,
			via:      text.via,
			more:     text.more,
			parts:    text.parts,
		})
	}
	return out
}

// moreText is what follows the models a "model M changed (through ...)"
// entry names when it leaves more of them out: ", and 3 more".
func moreText(more int) string {
	if more == 0 {
		return ""
	}
	return ", and " + strconv.Itoa(more) + " more"
}

// useText returns the text of the "model X changed" entry of u, built once
// per model and not; the entries share it, so treat it as read-only.
func (d *differ) useText(u refUse) useText {
	key := modelUse{name: u.name, not: u.not}
	text, ok := d.texts[key]
	if ok {
		return text
	}
	prefix := "model "
	if u.not {
		prefix = "not changed: model "
	}
	text.parts = []string{prefix, u.name, " changed"}
	st := d.models[u.name]
	if st != nil && len(st.changes) == 0 {
		text.via = d.sources(u.name)
		if len(text.via) > maxVia {
			text.more = len(text.via) - maxVia
			text.via = text.via[:maxVia]
		}
		text.parts[2] = " changed (through "
		for i, name := range text.via {
			if i > 0 {
				text.parts = append(text.parts, ", ")
			}
			text.parts = append(text.parts, name)
		}
		text.parts = append(text.parts, moreText(text.more)+")")
	}
	text.detail = strings.Join(text.parts, "")
	d.texts[key] = text
	return text
}

func (d *differ) modelDiff(name string, kind ChangeKind, usage map[useKey]bool) *ModelDiff {
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
			breaking = false
			for key := range usage {
				breaking = breaking || c.breakingAt(key.ctx, key.not, key.strict)
			}
		}
		md.Changes = append(md.Changes, Change{Kind: c.kind, Path: c.path.String(), Detail: c.detail, Breaking: breaking, parts: c.parts})
	}
	md.Changes = append(md.Changes, d.useChanges(st.uses, func(u refUse) bool {
		if len(usage) == 0 {
			return d.breaks(u.name, ctxModel, u.not, u.strict)
		}
		for key := range usage {
			if d.breaks(u.name, key.ctx, key.not != u.not, key.strict || u.strict) {
				return true
			}
		}
		return false
	})...)
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
			od.Changes = append(od.Changes, Change{Kind: c.kind, Path: c.path.String(), Detail: c.detail, Breaking: c.breaking(c.ctx), parts: c.parts})
		}
		od.Changes = append(od.Changes, d.useChanges(w.uses, func(u refUse) bool {
			return d.breaks(u.name, u.ctx, u.not, u.strict)
		})...)
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

func (w *walk) add(kind ChangeKind, path *diffPath, detail string, breakReq, breakResp bool) {
	w.addStrict(kind, path, detail, breakReq, breakResp, breakReq, breakResp)
}

// addStrict records a change that breaks a request as breakReq says and a
// response as breakResp says, and, judged strictly, as strictReq and
// strictResp say: what the values accepted do where the rules otherwise
// break nothing, enum values added and additional properties allowed again
// widening, a format added narrowing, one removed widening and one changed
// doing both. The strict flags are read where the model the change is in is
// referred to under a not, see rawChange.breakingAt, as what the not
// refuses moves with such changes too.
func (w *walk) addStrict(kind ChangeKind, path *diffPath, detail string, breakReq, breakResp, strictReq, strictResp bool) {
	w.changes = append(w.changes, rawChange{kind: kind, path: path, detail: detail, breakReq: breakReq, breakResp: breakResp, strictReq: strictReq, strictResp: strictResp, ctx: w.ctx})
}

func extend(path *diffPath, segs ...string) *diffPath {
	for _, seg := range segs {
		path = &diffPath{up: path, seg: seg}
	}
	return path
}

// itemsPath is the path of the items of path: [] appended to a property
// name, tags becoming tags[], and an items segment to a body or response,
// response 200 › items, see diffPath.String.
func itemsPath(path *diffPath) *diffPath {
	return &diffPath{up: path, items: true}
}

func (w *walk) operation(bref, href OperationRef, key string) {
	bop := bref.Operation
	hop := href.Operation
	root := rootPath(key)

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
		w.headers(bresp.Headers, hresp.Headers, path)
	}
}

// headers compares the headers of a response, matching names regardless of
// case as HTTP does. A removed header breaks clients that read it, and so
// does one that is no longer required; a header on both sides has its
// schema compared in response context. A header named Content-Type is
// skipped on both sides, as OpenAPI says to ignore its definition.
func (w *walk) headers(bh, hh Ordered[*Header], path *diffPath) {
	byName := func(headers Ordered[*Header]) map[string]string {
		out := map[string]string{}
		for _, name := range headers.Keys {
			out[strings.ToLower(name)] = name
		}
		return out
	}
	baseNames := byName(bh)
	headNames := byName(hh)

	for _, name := range bh.Keys {
		if strings.EqualFold(name, "Content-Type") {
			continue
		}
		_, ok := headNames[strings.ToLower(name)]
		if !ok {
			w.add(Removed, extend(path, "header "+name), "header removed", false, true)
		}
	}
	for _, name := range hh.Keys {
		if strings.EqualFold(name, "Content-Type") {
			continue
		}
		sub := extend(path, "header "+name)
		bname, ok := baseNames[strings.ToLower(name)]
		if !ok {
			w.add(Added, sub, "header added", false, false)
			continue
		}
		bhd := w.d.base.Header(bh.Map[bname])
		hhd := w.d.head.Header(hh.Map[name])
		if bhd == nil || hhd == nil {
			continue
		}
		if bhd.Required && !hhd.Required {
			w.add(Changed, sub, "header made optional", false, true)
		}
		if !bhd.Required && hhd.Required {
			w.add(Changed, sub, "header made required", false, false)
		}
		w.schema(parameterSchema(bhd), parameterSchema(hhd), sub, false)
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

func (w *walk) parameters(bref, href OperationRef, root *diffPath) {
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
		if w.serialization(bp, hp, path) {
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

// serialization records a change of how the parameter, bp in the base and
// hp in the head, is written, its style or explode with the defaults of its
// location filled in, and reports whether there is one. Either breaks a
// request, as a client writing the parameter the old way is misread,
// except a change of explode where both sides state a type that is neither
// an array nor an object, which every style writes the same exploded or
// not. A parameter described by content has no style and is skipped.
func (w *walk) serialization(bp, hp *Parameter, path *diffPath) bool {
	if bp.Content.Len() > 0 || hp.Content.Len() > 0 {
		return false
	}
	bStyle, bExplode := parameterStyle(bp)
	hStyle, hExplode := parameterStyle(hp)
	changed := false
	if bStyle != hStyle {
		w.add(Changed, path, "style changed from "+bStyle+" to "+hStyle, true, false)
		changed = true
	}
	if bExplode != hExplode {
		scalar := w.scalar(w.d.base, bp.Schema) && w.scalar(w.d.head, hp.Schema)
		w.add(Changed, path, "explode changed from "+strconv.FormatBool(bExplode)+" to "+strconv.FormatBool(hExplode), !scalar, false)
		changed = true
	}
	return changed
}

// parameterStyle returns the style and explode p is written with, the
// defaults of OpenAPI filled in: style form for a query or cookie parameter
// and simple for any other, explode true for style form and false for any
// other.
func parameterStyle(p *Parameter) (string, bool) {
	style := p.Style
	if style == "" {
		style = "simple"
		if p.In == "query" || p.In == "cookie" {
			style = "form"
		}
	}
	explode := style == "form"
	if p.Explode != nil {
		explode = *p.Explode
	}
	return style, explode
}

// scalar reports whether the flattened s, of doc, states a type, and only
// types that are neither array nor object, and has no variants.
func (w *walk) scalar(doc *Document, s *Schema) bool {
	f := w.d.flatten(doc, s)
	if f == nil || hasVariants(f) || typeText(f) == "" {
		return false
	}
	for _, t := range strings.Split(typeText(f), " | ") {
		if t == "array" || t == "object" {
			return false
		}
	}
	return true
}

func deprecationText(deprecated bool) string {
	if deprecated {
		return "deprecated"
	}
	return "no longer deprecated"
}

func (w *walk) requestBody(bb, hb *RequestBody, root *diffPath) {
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
func (w *walk) content(bc, hc Ordered[*MediaType], path *diffPath) {
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
	head   bool // type, format, nullable, description, a flag or a validation keyword changed
	enum   bool // enum values changed (the schema's own or its items')
	inside bool // nested properties, items or variants changed
}

// schema compares b with h and records the differences under path. rec
// enables row marks; it is turned off below a head schema that stands for a
// model, whose rows are marked by that model's own comparison.
func (w *walk) schema(b, h *Schema, path *diffPath, rec bool) schemaResult {
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

	// Both sides stand for the same model in a shape wrapper compares, under
	// any spelling of the reference. wrapper flattens the base side against
	// the head document, so its reference must lead to that model there too.
	// Where both sides refer first to the same component, that component is
	// the model on both sides, even when it is another name for a model on
	// one side and a model of its own on the other, as a component that only
	// refers to a model is until it gets a keyword of its own: the keyword is
	// the component's own change, reported once with the component, and the
	// use itself is the same.
	bn := modelName(w.d.base, b)
	hn := modelName(w.d.head, h)
	bh := modelName(w.d.head, b)
	first := refComponent(b)
	if first != "" && first == refComponent(h) && (bn != hn || bh != hn) {
		bn = first
		hn = first
		bh = first
	}
	if hn != "" && bn == hn && modelRef(b) != "" && modelRef(h) != "" && bh == hn {
		res.head = w.wrapper(b, h, path)
		w.uses = append(w.uses, refUse{name: hn, path: path, ctx: w.ctx})
		// A component that is only another name for the model, such as an
		// allOf of its one reference, named on either side, has its own entry
		// for the model's changes, judged where it is used.
		for _, name := range [2]string{first, refComponent(h)} {
			if name != "" && name != hn {
				w.quietUse(name, path)
			}
		}
		return res
	}
	if bn != hn && bn != "" && hn != "" {
		w.add(Changed, path, "schema changed from "+bn+" to "+hn, false, false)
		w.changes[len(w.changes)-1].parts = []string{"schema changed from ", bn, " to ", hn}
		res.head = true
	}
	if hn != "" {
		rec = false
	}
	w.mergedUses(h, path)

	// A 3.1 union of one schema and {type: null}, which Parse folds into
	// that schema made nullable, is compared as the list of that one
	// variant, under the other side's keyword, where the other side lists
	// variants here and that one schema has none once resolved, so a
	// variant added beside it reads as variants added, as in the 3.0
	// spelling. Anywhere else, as where its one schema is itself a union or
	// both sides are folded, the two are compared as they are.
	if folded(b) && hasVariants(h) && !hasVariants(w.d.flatten(w.d.base, b)) {
		b = w.d.unfold(b, h)
	} else if folded(h) && hasVariants(b) && !hasVariants(w.d.flatten(w.d.head, h)) {
		h = w.d.unfold(h, b)
	}

	br, _ := w.d.base.Schema(b)
	hr, _ := w.d.head.Schema(h)
	pair := [2]*Schema{br, hr}
	if w.pairs[pair] {
		return res
	}
	w.pairs[pair] = true
	defer delete(w.pairs, pair)

	bf := w.d.flatten(w.d.base, b)
	hf := w.d.flatten(w.d.head, h)
	if bf == nil || hf == nil {
		return res
	}

	if w.typeChange(w.d.base, bf, w.d.head, hf, path) {
		res.head = true
	}
	if bf.Format != hf.Format {
		w.addStrict(Changed, path, formatText(bf.Format, hf.Format), false, false, hf.Format != "", bf.Format != "")
		res.head = true
	}
	if w.nullable(w.d.base, bf, w.d.head, hf, path) {
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
	if w.validation(bf, hf, path) {
		res.head = true
	}

	if w.enum(bf.Enum, hf.Enum, path) {
		res.enum = true
	}
	if w.not(bf.Not, hf.Not, path) {
		res.head = true
	}
	if w.discriminator(bf.Discriminator, hf.Discriminator, path) {
		res.head = true
	}

	// Items on one side only are compared with the empty schema, which
	// allows any item, standing for the items keyword absent, as for
	// additional properties below, unless the type changed, which says
	// more. Items that allow nothing, items: false, say so instead.
	switch {
	case !refusesAll(bf.Items) && refusesAll(hf.Items):
		w.add(Changed, path, "items no longer allowed", true, false)
		res.inside = true
	case refusesAll(bf.Items) && !refusesAll(hf.Items):
		w.add(Changed, path, "items allowed", false, true)
		res.inside = true
	case bf.Items != nil && hf.Items != nil:
		sub := w.schema(bf.Items, hf.Items, itemsPath(path), rec)
		res.head = res.head || sub.head
		res.enum = res.enum || sub.enum
		res.inside = res.inside || sub.inside
	case (bf.Items != nil || hf.Items != nil) && typeText(bf) == typeText(hf):
		sub := w.schema(itemsSchema(bf), itemsSchema(hf), itemsPath(path), false)
		if sub.head || sub.enum || sub.inside {
			res.inside = true
		}
	}

	if w.properties(bf, hf, path, rec) {
		res.inside = true
	}

	// An object is closed, additionalProperties: false, or open, and then the
	// schema of its additional properties is what the keyword holds, the
	// empty schema when it is absent or true. Closing an object refuses the
	// extra properties a client may have sent; opening one only lets a
	// response carry more, which, like a new response property, is not
	// breaking. Two open objects have the schemas of their additional
	// properties compared like any others.
	switch {
	case !closed(bf) && closed(hf):
		w.add(Changed, path, "additional properties no longer allowed", true, false)
		res.inside = true
	case closed(bf) && !closed(hf):
		w.addStrict(Changed, path, "additional properties allowed", false, false, false, true)
		res.inside = true
	case !closed(bf) && (bf.AdditionalSchema() != nil || hf.AdditionalSchema() != nil):
		sub := w.schema(additionalSchema(bf), additionalSchema(hf), extend(path, "<key>"), false)
		if sub.head || sub.enum || sub.inside {
			res.inside = true
		}
	}

	if w.variants(b, h, bf, hf, path, rec) {
		res.inside = true
	}
	return res
}

// wrapper compares what applies where both sides refer to the same model
// with modelRef's shape: the keywords beside the reference merged with the
// model. Each keyword is decided on its own. It counts as changed at the use
// site when the base side and the head side differ against the same model,
// the head's, so a keyword only the model changed is reported once, through
// the model. A keyword the site did change is reported from what held there
// before, the base side with the base document's model, so a bound changed
// at the site reads from the bound that applied, not from the model's new
// one. Two sides written the same, the common case, differ against the same
// model in nothing, so they are not flattened at all.
func (w *walk) wrapper(b, h *Schema, path *diffPath) bool {
	if sameSite(b, h) {
		return false
	}
	bh := w.d.flatten(w.d.head, b)
	bb := w.d.flatten(w.d.base, b)
	hf := w.d.flatten(w.d.head, h)
	if bh == nil || bb == nil || hf == nil {
		return false
	}
	return w.keywords(siteBefore(bh, bb, hf), hf, path)
}

// sameSite reports whether b and h, both of modelRef's shape, are written
// the same: the same $ref and the same keywords at each level.
func sameSite(b, h *Schema) bool {
	for hops := 0; b != nil && h != nil && hops < 32; hops++ {
		if b.Ref != h.Ref || len(b.AllOf) != len(h.AllOf) || !sameKeywords(b, h) {
			return false
		}
		if len(b.AllOf) != 1 {
			return len(b.AllOf) == 0
		}
		b = b.AllOf[0]
		h = h.AllOf[0]
	}
	return false
}

// sameKeywords reports whether b and h, neither of which describes a
// structure, see hasStructure, set the same keywords other than $ref and
// allOf. Extensions are not keywords and are left out. Each field is
// compared by name, as reflection over them all costs more than the rest of
// a use of a model.
func sameKeywords(b, h *Schema) bool {
	return b.Title == h.Title && b.Description == h.Description && slices.Equal(b.Type, h.Type) && b.Format == h.Format &&
		b.Nullable == h.Nullable && b.NullMember == h.NullMember && b.NullOnly == h.NullOnly && b.Deprecated == h.Deprecated &&
		b.ReadOnly == h.ReadOnly && b.WriteOnly == h.WriteOnly && b.UniqueItems == h.UniqueItems && b.Pattern == h.Pattern &&
		samePtr(b.Minimum, h.Minimum) && samePtr(b.Maximum, h.Maximum) && samePtr(b.MultipleOf, h.MultipleOf) &&
		sameBound(b.ExclusiveMinimum, h.ExclusiveMinimum) && sameBound(b.ExclusiveMaximum, h.ExclusiveMaximum) &&
		samePtr(b.MinLength, h.MinLength) && samePtr(b.MaxLength, h.MaxLength) &&
		samePtr(b.MinItems, h.MinItems) && samePtr(b.MaxItems, h.MaxItems) &&
		samePtr(b.MinProperties, h.MinProperties) && samePtr(b.MaxProperties, h.MaxProperties) &&
		reflect.DeepEqual(b.Const, h.Const) && b.ConstNull == h.ConstNull && reflect.DeepEqual(b.Default, h.Default) && reflect.DeepEqual(b.Example, h.Example) &&
		(b.Examples == nil && h.Examples == nil || reflect.DeepEqual(b.Examples, h.Examples))
}

func samePtr[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func sameBound(a, b *Bound) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Flag == b.Flag && samePtr(a.Value, b.Value)
}

// siteKeywords holds the indexes of the Schema fields keywords compares,
// grouped where one keyword is read from two fields.
var siteKeywords = func() [][]int {
	groups := [][]string{
		{"Format"}, {"Description"}, {"Nullable"}, {"Deprecated"}, {"ReadOnly"}, {"WriteOnly"},
		{"Minimum", "ExclusiveMinimum"}, {"Maximum", "ExclusiveMaximum"},
		{"MultipleOf"}, {"MinLength"}, {"MaxLength"}, {"Pattern"}, {"MinItems"}, {"MaxItems"},
		{"UniqueItems"}, {"MinProperties"}, {"MaxProperties"}, {"Const", "ConstNull"}, {"Default"},
	}
	t := reflect.TypeOf(Schema{})
	out := make([][]int, len(groups))
	for i, group := range groups {
		for _, name := range group {
			field, ok := t.FieldByName(name)
			if !ok {
				panic("reference: Schema has no field " + name)
			}
			out[i] = append(out[i], field.Index[0])
		}
	}
	return out
}()

// siteBefore returns what held at a use site before: the head side hf with,
// for each keyword the site itself changed, the value of bb, the base side
// with the base document's model. The site changed a keyword when bh, the
// base side with the head's model, differs from hf in it.
func siteBefore(bh, bb, hf *Schema) *Schema {
	before := *hf
	if typeText(bh) != typeText(hf) {
		before.Type = bb.Type
		before.Items = bb.Items
		before.Properties = bb.Properties
		before.AdditionalProperties = bb.AdditionalProperties
	}
	out := reflect.ValueOf(&before).Elem()
	same := reflect.ValueOf(bh).Elem()
	head := reflect.ValueOf(hf).Elem()
	base := reflect.ValueOf(bb).Elem()
	for _, group := range siteKeywords {
		changed := false
		for _, i := range group {
			if !reflect.DeepEqual(same.Field(i).Interface(), head.Field(i).Interface()) {
				changed = true
			}
		}
		if !changed {
			continue
		}
		for _, i := range group {
			out.Field(i).Set(base.Field(i))
		}
	}
	return &before
}

// keywords records the differences between b and h, flattened sides of a
// use of one model, in the keywords wrapper compares, and reports whether
// there are any.
func (w *walk) keywords(b, h *Schema, path *diffPath) bool {
	changed := false
	// Both sides hold the head document's model, and so its variants.
	if w.typeChange(w.d.head, b, w.d.head, h, path) {
		changed = true
	}
	if b.Format != h.Format {
		w.addStrict(Changed, path, formatText(b.Format, h.Format), false, false, h.Format != "", b.Format != "")
		changed = true
	}
	if b.Description != h.Description {
		w.add(Changed, path, "description changed", false, false)
		changed = true
	}
	if w.nullable(w.d.head, b, w.d.head, h, path) {
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
	if w.validation(b, h, path) {
		changed = true
	}
	return changed
}

// typeChange records a change of type between the flattened b, of the
// document bd, and h, of hd, and reports whether there is one. A type on
// one side only, where both sides have oneOf or anyOf variants that the
// type restates, see restates, as a type beside a reference to a model that
// is a oneOf of objects does, changes nothing. Otherwise a type stated where there
// was none narrows the values accepted and one dropped widens them; a
// change between two stated types breaks both. A type of only null, see
// typeName, is a stated type that accepts only what the change of nullable
// reports, so a change to it only narrows and one from it only widens.
func (w *walk) typeChange(bd *Document, b *Schema, hd *Document, h *Schema, path *diffPath) bool {
	bt := typeName(b)
	ht := typeName(h)
	if bt == ht || slices.Equal(typeSet(bt), typeSet(ht)) {
		return false
	}
	if hasVariants(b) && hasVariants(h) && (bt == "" && w.restates(hd, h) || ht == "" && w.restates(bd, b)) {
		return false
	}
	w.add(Changed, path, "type changed from "+quoteType(bt)+" to "+quoteType(ht), ht != "" && bt != "null", bt != "" && ht != "null")
	return true
}

// typeSet is the type text t, see typeText, as a sorted set of types, so
// two lists of the same types in another order are the same.
func typeSet(t string) []string {
	return slices.Compact(slices.Sorted(slices.Values(strings.Split(t, " | "))))
}

// typeName is the type typeChange reads from the flattened f: typeText, or
// null where null is the only type it lists, see Schema.NullOnly, so that a
// change to or from only null is read as a change of type, unlike one to or
// from a schema that states no type.
func typeName(f *Schema) string {
	if f.NullOnly && len(f.Type) == 0 {
		return "null"
	}
	return typeText(f)
}

// restates reports whether the type of the flattened f, of the document d,
// takes nothing away from its oneOf and anyOf variants: each of them states
// a type, every non-null type it states is one of f's, an integer counting
// as a number, and none lets null through where f does not.
func (w *walk) restates(d *Document, f *Schema) bool {
	types := strings.Split(typeText(f), " | ")
	for _, v := range slices.Concat(f.OneOf, f.AnyOf) {
		vf := w.d.flatten(d, v)
		if vf == nil || typeText(vf) == "" || vf.Nullable && !f.Nullable {
			return false
		}
		for _, t := range strings.Split(typeText(vf), " | ") {
			if !slices.Contains(types, t) && (t != "integer" || !slices.Contains(types, "number")) {
				return false
			}
		}
	}
	return true
}

// nullable records a change of nullable between the flattened b, of bd, and
// h, of hd, and reports whether there is one. It narrows the values accepted
// only when h refuses null where b let it through, and widens them only in
// the reverse case, as a schema that states no type lets null through with
// nullable or without it, see letsNull.
func (w *walk) nullable(bd *Document, b *Schema, hd *Document, h *Schema, path *diffPath) bool {
	if b.Nullable == h.Nullable {
		return false
	}
	bNull := w.letsNull(bd, b, map[*Schema]bool{})
	hNull := w.letsNull(hd, h, map[*Schema]bool{})
	narrows := bNull && !hNull
	widens := !bNull && hNull
	if h.Nullable {
		w.add(Changed, path, "made nullable", narrows, widens)
	} else {
		w.add(Changed, path, "no longer nullable", narrows, widens)
	}
	return true
}

// admitsNull reports whether the flattened schema f lets null through: it
// is nullable, or it states no type and none of enum, a const other than
// null, oneOf, anyOf and not, which may refuse null. No type is inferred from properties or
// items here, as those apply to objects and arrays only. An enum without
// null beside nullable is still read as letting null through.
func admitsNull(f *Schema) bool {
	return f.Nullable || len(f.Type) == 0 && len(f.Enum) == 0 && f.Const == nil && !hasVariants(f) && f.Not == nil
}

// letsNull reports whether the flattened schema f, of doc, lets null
// through: it is nullable, or it states no type, no not, no const other than
// null, an enum only where it lists
// null, and oneOf or anyOf variants only where one of each keyword's lets
// null through. A not is read as refusing null whatever it holds. seen holds
// what each variant asked about was found to do, and a variant asked about
// again while it is being asked about, through a cycle, is read as refusing
// null.
func (w *walk) letsNull(doc *Document, f *Schema, seen map[*Schema]bool) bool {
	if f.Nullable {
		return true
	}
	if len(f.Type) > 0 || f.Const != nil || f.Not != nil {
		return false
	}
	if len(f.Enum) > 0 && !slices.Contains(f.Enum, nil) {
		return false
	}
	for _, list := range [][]*Schema{f.OneOf, f.AnyOf} {
		if len(list) == 0 {
			continue
		}
		some := false
		for _, v := range list {
			known, ok := seen[v]
			if !ok {
				seen[v] = false
				vf := w.d.flatten(doc, v)
				known = vf != nil && w.letsNull(doc, vf, seen)
				seen[v] = known
			}
			if known {
				some = true
				break
			}
		}
		if !some {
			return false
		}
	}
	return true
}

// enum records a change of the enum values from b to h and reports whether
// there is one. An enum where there was none narrows the values accepted
// and one dropped widens them; between two enums, a value removed narrows,
// and a value added, which only lets a response carry more, as a new
// response property does, is not breaking.
func (w *walk) enum(b, h []any, path *diffPath) bool {
	added, removed := enumDelta(b, h)
	switch {
	case len(added) == 0 && len(removed) == 0:
		return false
	case len(b) == 0:
		w.add(Changed, path, "enum added: "+enumList(added), true, false)
	case len(h) == 0:
		w.add(Changed, path, "enum removed (was "+enumList(removed)+")", false, true)
	default:
		w.addStrict(Changed, path, enumText(added, removed), len(removed) > 0, false, len(removed) > 0, len(added) > 0)
	}
	return true
}

// not records a change of the not keyword from b to h and reports whether
// there is one. A not given where there was none narrows the values
// accepted and one dropped widens them. Between two, the schemas under the
// nots are compared as written, see writtenKey, and never walked, so the
// comparison is linear in what is written there: a $ref is the same as
// another where it names the same component, whose model is not compared
// there. Written the same, they report nothing; different only in the
// annotations, which the diff never flags as breaking, title, description,
// default, example, examples, deprecated, readOnly and writeOnly, not
// changed, breaking neither; different otherwise, not changed, breaking
// both, as the diff does not work out which way a change under a not
// moves. Extensions are left out, as everywhere. The models either side
// names under the not are recorded as used here, under one more not, see
// namedUses, so a change to one reads not changed: model M changed, or model
// M changed under an even number of nots in all, judged as
// rawChange.breakingAt says.
func (w *walk) not(b, h *Schema, path *diffPath) bool {
	switch {
	case b == nil && h == nil:
		return false
	case b == nil:
		w.add(Changed, path, "not added", true, false)
		return true
	case h == nil:
		w.add(Changed, path, "not removed", false, true)
		return true
	}
	w.namedUses(h, path, true, true)
	w.namedUses(b, path, true, true)
	if w.d.key(b) != w.d.key(h) {
		w.add(Changed, path, "not changed", true, true)
		return true
	}
	if annotatedKey(b) == annotatedKey(h) {
		return false
	}
	w.add(Changed, path, "not changed", false, false)
	return true
}

// discriminator records a change of the discriminator from b to h and
// reports whether there is one. A discriminator given where there was none
// narrows the values accepted, as the property it names must then pick a
// variant, and one dropped widens them; a changed propertyName breaks both.
// The mapping is compared as written, by the component each value names,
// and the names of variants it leaves implicit are not compared: a value
// added, like an enum value added, breaks neither, one removed narrows, and
// one that names another component breaks both.
func (w *walk) discriminator(b, h *Discriminator, path *diffPath) bool {
	switch {
	case b == nil && h == nil:
		return false
	case b == nil:
		w.add(Changed, path, "discriminator added: "+h.PropertyName, true, false)
		return true
	case h == nil:
		w.add(Changed, path, "discriminator removed (was "+b.PropertyName+")", false, true)
		return true
	}
	n := len(w.changes)
	if b.PropertyName != h.PropertyName {
		w.add(Changed, path, "discriminator property changed from "+b.PropertyName+" to "+h.PropertyName, true, true)
	}
	keys := slices.Collect(maps.Keys(b.Mapping))
	for key := range h.Mapping {
		_, ok := b.Mapping[key]
		if !ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		bt, inBase := b.Mapping[key]
		ht, inHead := h.Mapping[key]
		value := valueText(key)
		switch {
		case !inBase:
			w.addStrict(Changed, path, "discriminator mapping added: "+value+" → "+mappingName(ht), false, false, false, true)
		case !inHead:
			w.add(Changed, path, "discriminator mapping removed: "+value+" → "+mappingName(bt), true, false)
		case mappingName(bt) != mappingName(ht):
			w.add(Changed, path, "discriminator mapping "+value+" changed from "+mappingName(bt)+" to "+mappingName(ht), true, true)
		}
	}
	return len(w.changes) > n
}

// mappingName is the component a discriminator mapping value names: the
// name a schema reference points at, or the value as written, which is a
// component name or a reference elsewhere.
func mappingName(target string) string {
	name := RefName(target, refSchemas)
	if name == "" {
		return target
	}
	return name
}

// refusesAll reports whether s is the schema that allows no value,
// {not: {}}, which Parse makes of the boolean schema false, with nothing
// but annotations beside the not or under it.
func refusesAll(s *Schema) bool {
	if s == nil || s.Not == nil || !allowsAll(s.Not) {
		return false
	}
	rest := *s
	rest.Not = nil
	return allowsAll(&rest)
}

// allowsAll reports whether s, nil standing for the keyword absent, is the
// empty schema, which allows any value, give or take annotations: title,
// description, default, example, examples, deprecated, readOnly and
// writeOnly, which describe values without limiting them.
func allowsAll(s *Schema) bool {
	if s == nil {
		return true
	}
	rest := *s
	rest.Title = ""
	rest.Description = ""
	rest.Default = nil
	rest.Example = nil
	rest.Examples = nil
	rest.Deprecated = false
	rest.ReadOnly = false
	rest.WriteOnly = false
	return rest.Ref == "" && len(rest.AllOf) == 0 && !hasKeywords(&rest)
}

// closed reports whether s refuses properties it does not list, with
// additionalProperties: false, or a schema that allows no value.
func closed(s *Schema) bool {
	return refusesExtra(s.AdditionalProperties)
}

// anySchema is the empty schema, which allows any value, standing for an
// absent keyword in the comparison. It is one schema for every such keyword,
// so the walk's cycle guard, keyed by schema, sees a comparison of it with
// the same head schema again as the cycle it is. Treat it as read-only.
var anySchema = &Schema{}

// additionalSchema returns the schema of the additional properties of s, an
// open object: what additionalProperties holds, or the empty schema, which
// allows any value, when the keyword is absent or true.
func additionalSchema(s *Schema) *Schema {
	a := s.AdditionalSchema()
	if a == nil {
		return anySchema
	}
	return a
}

// itemsSchema returns the schema of the items of s: what items holds, or the
// empty schema, which allows any item, when the keyword is absent.
func itemsSchema(s *Schema) *Schema {
	if s.Items == nil {
		return anySchema
	}
	return s.Items
}

// modelName is the component schema s stands for, as SchemaName says, but
// with the keywords beside a $ref applying: a component that is a $ref with
// keywords beside it, or an allOf of one with keywords beside the allOf, is
// a model of its own, while one that is only a reference, in either form,
// is another name for the model it refers to; a $ref with an allOf beside
// it is an allOf of several schemas and stands for no single model. Two
// spellings of one schema then stand for the same model. A schema with a
// structure of its own around the reference, such as properties beside a
// single-member allOf, stands for no model either: it is more than the
// model it refers to.
func modelName(d *Document, s *Schema) string {
	name := ""
	for hops := 0; s != nil && hops < 32; hops++ {
		if name == "" && hasStructure(s) {
			return ""
		}
		if s.Ref == "" {
			if len(s.AllOf) != 1 || name != "" && hasKeywords(s) {
				return name
			}
			s = s.AllOf[0]
			continue
		}
		if name != "" && hasSiblings(s) || len(s.AllOf) > 0 {
			return name
		}
		target := RefName(s.Ref, refSchemas)
		if target == "" {
			return s.Ref
		}
		next, ok := d.Components.Schemas.Get(target)
		if !ok || next == nil {
			return target
		}
		name = target
		s = next
	}
	return name
}

// modelRef returns the $ref of s when s has a shape wrapper compares: a
// $ref, or an allOf whose only member has that shape, so wrappers nest
// around one reference, with or without keywords beside the reference and
// on each wrapper, none of which describes a structure, which wrapper does
// not compare. A type or a format beside the reference gives it no
// structure of its own, as in the 3.0 spelling of a nullable reference,
// allOf: [{$ref}] with nullable and type: object. It returns "" for any
// other shape, which is compared in full.
func modelRef(s *Schema) string {
	for hops := 0; s != nil && hops < 32; hops++ {
		if hasStructure(s) {
			return ""
		}
		if s.Ref != "" {
			if len(s.AllOf) > 0 {
				return ""
			}
			return s.Ref
		}
		if len(s.AllOf) != 1 {
			return ""
		}
		s = s.AllOf[0]
	}
	return ""
}

// refComponent is the component schema the reference of s names, in the
// shape modelRef accepts, before any reference that component holds is
// followed, or "" when s has no such reference.
func refComponent(s *Schema) string {
	return RefName(modelRef(s), refSchemas)
}

// hasStructure reports whether s describes a structure of its own, with
// any of properties, required, items, enum, oneOf, anyOf, not,
// discriminator and additionalProperties, which wrapper does not compare.
func hasStructure(s *Schema) bool {
	return len(s.Enum) > 0 || s.Properties.Len() > 0 || len(s.Required) > 0 || s.Items != nil ||
		len(s.OneOf) > 0 || len(s.AnyOf) > 0 || s.Not != nil || s.Discriminator != nil || s.AdditionalProperties != nil
}

// validation compares the validation keywords of b and h and reports whether
// any differ. A change that narrows the values a schema accepts breaks a
// request, where a client sending what used to be valid is now refused; one
// that widens them breaks a response, where a client may receive what it was
// never told to expect; one whose direction cannot be told breaks both.
func (w *walk) validation(b, h *Schema, path *diffPath) bool {
	n := len(w.changes)
	w.bound(path, "minimum", knownLower(b), knownLower(h), integral(b), integral(h), true)
	w.bound(path, "maximum", knownUpper(b), knownUpper(h), integral(b), integral(h), false)
	w.keyword(path, "multipleOf", numberText(b.MultipleOf), numberText(h.MultipleOf), true)
	w.count(path, "minLength", b.MinLength, h.MinLength, false)
	w.count(path, "maxLength", b.MaxLength, h.MaxLength, true)
	w.keyword(path, "pattern", patternText(b.Pattern), patternText(h.Pattern), true)
	w.count(path, "minItems", b.MinItems, h.MinItems, false)
	w.count(path, "maxItems", b.MaxItems, h.MaxItems, true)
	if b.UniqueItems != h.UniqueItems {
		if h.UniqueItems {
			w.add(Changed, path, "uniqueItems added", true, false)
		} else {
			w.add(Changed, path, "uniqueItems removed", false, true)
		}
	}
	w.count(path, "minProperties", b.MinProperties, h.MinProperties, false)
	w.count(path, "maxProperties", b.MaxProperties, h.MaxProperties, true)
	w.keyword(path, "const", constText(b), constText(h), true)
	w.keyword(path, "default", valueText(b.Default), valueText(h.Default), false)
	return len(w.changes) > n
}

// numBound is an effective numeric bound: its value and whether the value
// itself is excluded.
type numBound struct {
	value     float64
	exclusive bool
}

func (nb numBound) text() string {
	if nb.exclusive {
		return formatNumber(nb.value) + " exclusive"
	}
	return formatNumber(nb.value)
}

// tighter reports whether nb admits fewer values than other, both lower
// bounds when lower is set and upper bounds otherwise.
func (nb numBound) tighter(other numBound, lower bool) bool {
	if nb.value != other.value {
		return (nb.value > other.value) == lower
	}
	return nb.exclusive && !other.exclusive
}

func lowerBound(s *Schema) *numBound {
	return effectiveBound(s.Minimum, s.ExclusiveMinimum, true)
}

func upperBound(s *Schema) *numBound {
	return effectiveBound(s.Maximum, s.ExclusiveMaximum, false)
}

// knownLower is lowerBound with a bound that restricts nothing read as no
// bound: NaN, which no number can be compared with, and minus infinity.
func knownLower(s *Schema) *numBound {
	return effectiveBound(knownValue(s.Minimum, -1), knownExclusive(s.ExclusiveMinimum, -1), true)
}

// knownUpper is upperBound with NaN and plus infinity read as no bound.
func knownUpper(s *Schema) *numBound {
	return effectiveBound(knownValue(s.Maximum, 1), knownExclusive(s.ExclusiveMaximum, 1), false)
}

// knownValue returns v, or nil when it is NaN or the infinity of sign,
// which restricts nothing.
func knownValue(v *float64, sign int) *float64 {
	if v != nil && (math.IsNaN(*v) || math.IsInf(*v, sign)) {
		return nil
	}
	return v
}

func knownExclusive(b *Bound, sign int) *Bound {
	if b != nil && b.Value != nil && (math.IsNaN(*b.Value) || math.IsInf(*b.Value, sign)) {
		return nil
	}
	return b
}

// effectiveBound folds minimum and exclusiveMinimum, or maximum and
// exclusiveMaximum, into one bound, nil when there is none. Parse turns the
// 3.0 form, a boolean that makes minimum exclusive, into the 3.1 number; a
// document built without Parse may still hold the boolean and is read the
// same way. When both keywords hold a number the tighter one applies.
func effectiveBound(limit *float64, excl *Bound, lower bool) *numBound {
	var inclusive, exclusive *numBound
	if limit != nil {
		inclusive = &numBound{value: *limit, exclusive: excl != nil && excl.Value == nil && excl.Flag}
	}
	if excl != nil && excl.Value != nil {
		exclusive = &numBound{value: *excl.Value, exclusive: true}
	}
	return tighterBound(inclusive, exclusive, lower)
}

// tighterBound returns the tighter of two lower bounds (lower set) or upper
// bounds, either of which may be nil, and a at equal ones.
func tighterBound(a, b *numBound, lower bool) *numBound {
	if a == nil || b != nil && b.tighter(*a, lower) {
		return b
	}
	return a
}

// bound compares a lower bound (lower set) or an upper bound: a tighter
// bound narrows, a looser one widens, and none admits every number. A bound
// of a side whose type is integer is compared as the integer bound it
// amounts to, see integerBound, and written as given, followed by that
// integer bound when only one side is integer and it differs, so the words
// raised and lowered agree with the numbers shown.
func (w *walk) bound(path *diffPath, name string, b, h *numBound, bInt, hInt, lower bool) {
	narrowed, widened := "raised", "lowered"
	if !lower {
		narrowed, widened = widened, narrowed
	}
	switch {
	case b == nil && h == nil:
		return
	case b == nil:
		w.add(Changed, path, name+" added: "+h.text(), true, false)
		return
	case h == nil:
		w.add(Changed, path, name+" removed (was "+b.text()+")", false, true)
		return
	}
	bc := integerBound(*b, bInt, lower)
	hc := integerBound(*h, hInt, lower)
	if bc == hc {
		return
	}
	bt := b.text()
	ht := h.text()
	if bInt != hInt {
		bt = comparedText(*b, bc)
		ht = comparedText(*h, hc)
	}
	if hc.tighter(bc, lower) {
		w.add(Changed, path, name+" "+narrowed+" from "+bt+" to "+ht, true, false)
	} else {
		w.add(Changed, path, name+" "+widened+" from "+bt+" to "+ht, false, true)
	}
}

// comparedText writes the bound nb, followed by c, the integer bound it was
// compared as, when that differs: "0.5 (1 for integers)".
func comparedText(nb, c numBound) string {
	if nb == c {
		return nb.text()
	}
	return nb.text() + " (" + c.text() + " for integers)"
}

// integral reports whether the flattened schema s admits integers and no
// other numbers, so its numeric bounds only ever meet integers.
func integral(s *Schema) bool {
	return s.Type.Has("integer") && !s.Type.Has("number")
}

// integerBound returns nb, a lower bound (lower set) or an upper bound, as
// the inclusive bound that admits the same integers when integer is set:
// an exclusive lower bound v is floor(v)+1 and an inclusive one ceil(v), an
// exclusive upper bound ceil(v)-1 and an inclusive one floor(v), so
// minimum: 1 and exclusiveMinimum: 0 compare equal. A bound of magnitude
// 2^53 or more, where float64 has no next integer, is returned as it is.
func integerBound(nb numBound, integer, lower bool) numBound {
	if !integer || math.Abs(nb.value) >= 1<<53 {
		return nb
	}
	v := nb.value
	switch {
	case lower && nb.exclusive:
		v = math.Floor(v) + 1
	case lower:
		v = math.Ceil(v)
	case nb.exclusive:
		v = math.Ceil(v) - 1
	default:
		v = math.Floor(v)
	}
	return numBound{value: v}
}

// count compares a minLength, minItems or minProperties keyword, where none
// means 0, or, with upper set, a maxLength, maxItems or maxProperties
// keyword, where none means no limit. A raised minimum or a lowered maximum
// narrows; the reverse widens.
func (w *walk) count(path *diffPath, name string, b, h *int, upper bool) {
	if b == nil && h == nil || b != nil && h != nil && *b == *h {
		return
	}
	var detail string
	switch {
	case b == nil:
		detail = name + " added: " + strconv.Itoa(*h)
	case h == nil:
		detail = name + " removed (was " + strconv.Itoa(*b) + ")"
	case *h > *b:
		detail = name + " raised from " + strconv.Itoa(*b) + " to " + strconv.Itoa(*h)
	default:
		detail = name + " lowered from " + strconv.Itoa(*b) + " to " + strconv.Itoa(*h)
	}
	var narrows, widens bool
	if upper {
		narrows = h != nil && (b == nil || *h < *b)
		widens = b != nil && (h == nil || *h > *b)
	} else {
		lo := func(v *int) int {
			if v == nil {
				return 0
			}
			return *v
		}
		narrows = lo(h) > lo(b)
		widens = lo(h) < lo(b)
	}
	w.add(Changed, path, detail, narrows, widens)
}

// keyword compares a keyword given as text, "" when unset. With directed
// set, as for a pattern, an added keyword narrows, a removed one widens and
// a replaced one, whose direction cannot be told, breaks both; otherwise,
// as for a default, the change is never breaking.
func (w *walk) keyword(path *diffPath, name, b, h string, directed bool) {
	switch {
	case b == h:
	case b == "":
		w.add(Changed, path, name+" added: "+clip(h), directed, false)
	case h == "":
		w.add(Changed, path, name+" removed (was "+clip(b)+")", false, directed)
	default:
		w.add(Changed, path, name+" changed from "+clip(b)+" to "+clip(h), directed, directed)
	}
}

func numberText(f *float64) string {
	if f == nil {
		return ""
	}
	return formatNumber(*f)
}

// valueText is v as JSON, so values decoded from YAML and from JSON compare
// equal, or "" when v is unset. <, > and & are written as they are, since
// the text is read by people and escaped wherever it is written.
func valueText(v any) string {
	if v == nil {
		return ""
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(v)
	if err != nil {
		return ParamString(v)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// constText is the const of s as JSON, null for const: null, or "" when
// there is none.
func constText(s *Schema) string {
	if s.ConstNull && s.Const == nil {
		return "null"
	}
	return valueText(s.Const)
}

// patternText is a pattern as a JSON string, so where it starts and ends is
// clear and a line break in it reads \n, or "" when there is none.
func patternText(pattern string) string {
	if pattern == "" {
		return ""
	}
	return valueText(pattern)
}

func flagText(flag string, set bool) string {
	if set {
		return "made " + flag
	}
	return "no longer " + flag
}

// properties compares the properties of two flattened objects and reports
// whether any differ. What it records beside the comparison of each
// property on both sides is done in functions of its own, so each level of
// a deep comparison takes little stack.
func (w *walk) properties(bf, hf *Schema, path *diffPath, rec bool) bool {
	changed := w.removedProperties(bf, hf, path, rec)
	for _, key := range hf.Properties.Keys {
		bp, ok := bf.Properties.Get(key)
		if !ok {
			w.addedProperty(hf, key, path, rec)
			changed = true
			continue
		}
		hp := hf.Properties.Map[key]
		sub := extend(path, key)
		head := w.requiredChange(bf, hf, key, sub)
		res := w.schema(bp, hp, sub, rec)
		if head || res.head || res.enum || res.inside {
			changed = true
		}
		if rec && hp != nil {
			w.markProperty(hp, bp, key, bf.IsRequired(key), head || res.head, res)
		}
	}
	return w.requiredOnly(bf, hf, path) || changed
}

// removedProperties records the properties of bf that hf no longer lists,
// with their rows where rec is set, and reports whether there are any.
func (w *walk) removedProperties(bf, hf *Schema, path *diffPath, rec bool) bool {
	changed := false
	accepted := false
	marks := w.d.marks
	for i, key := range bf.Properties.Keys {
		_, ok := hf.Properties.Get(key)
		if ok {
			continue
		}
		changed = true
		// An object that refuses what the property held, as a closed one
		// refuses any property it does not list, refuses a request that
		// still sends it.
		refuses := w.refusesRemoved(bf.Properties.Map[key], hf)
		w.add(Removed, extend(path, key), "property removed", refuses, true)
		// Accepted now by the schema of the additional properties, written
		// as the property's was, which schema walks only where bf allowed
		// additional properties too, the property's value reaches the models
		// that schema names, so their changes reach this use.
		if !refuses && !accepted && closed(bf) && !allowsAll(hf.AdditionalSchema()) {
			w.namedUses(hf.AdditionalSchema(), extend(path, "<key>"), false, false)
			accepted = true
		}
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
	return changed
}

// addedProperty records the property key of hf, which bf does not list,
// with its row where rec is set.
func (w *walk) addedProperty(hf *Schema, key string, path *diffPath, rec bool) {
	required := hf.IsRequired(key)
	detail := "property added"
	if required {
		detail = "required property added"
	}
	w.add(Added, extend(path, key), detail, required, false)
	hp := hf.Properties.Map[key]
	if rec && hp != nil {
		w.d.marks.mark(hp).added = true
	}
}

// requiredChange records whether the property key, on both sides, was made
// required or optional, at path, and reports whether it was.
func (w *walk) requiredChange(bf, hf *Schema, key string, path *diffPath) bool {
	wasRequired := bf.IsRequired(key)
	required := hf.IsRequired(key)
	switch {
	case !wasRequired && required:
		w.add(Changed, path, "made required", true, false)
	case wasRequired && !required:
		w.add(Changed, path, "made optional", false, true)
	default:
		return false
	}
	return true
}

// markProperty records how the row of the property key, hp in the head and
// bp in the base, shows its change, see markRow.
func (w *walk) markProperty(hp, bp *Schema, key string, wasRequired, head bool, res schemaResult) {
	w.markRow(hp, rowSpec{name: key, schema: bp, required: wasRequired}, head, res, bp)
}

// requiredOnly records the names required on one side without a property of
// that name on either, and reports whether there are any. A name required
// without a property of that name in the head still requires that property
// of a request, and one no longer required lets a response leave it out.
func (w *walk) requiredOnly(bf, hf *Schema, path *diffPath) bool {
	changed := false
	for _, key := range hf.Required {
		_, ok := hf.Properties.Get(key)
		if ok || bf.IsRequired(key) {
			continue
		}
		changed = true
		w.add(Changed, extend(path, key), "made required", true, false)
	}
	for _, key := range bf.Required {
		_, inHead := hf.Properties.Get(key)
		_, inBase := bf.Properties.Get(key)
		if inHead || inBase || hf.IsRequired(key) {
			continue
		}
		changed = true
		w.add(Changed, extend(path, key), "made optional", false, true)
	}
	return changed
}

// refusesRemoved reports whether hf, which no longer lists a property whose
// schema was bp, refuses a value bp accepted under that name, judged from
// what is written: hf is closed, or the schema of its additional properties
// limits values at all and is not written as bp is, annotations left out
// and a $ref the same as another where it names the same component, see
// writtenKey. A schema written otherwise that still accepts every value bp
// does is read as refusing one, as working that out means comparing the
// two.
func (w *walk) refusesRemoved(bp, hf *Schema) bool {
	if closed(hf) {
		return true
	}
	a := hf.AdditionalSchema()
	if allowsAll(a) {
		return false
	}
	return w.d.key(bp) != w.d.key(a)
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

// variants compares the oneOf and anyOf variants of the flattened bf and
// hf, written as b and h, and reports whether they changed. Each keyword is
// compared on its own, except where one side has only oneOf and the other
// only anyOf. Lists of the same keyword and length are compared variant by
// variant, by position. Any other change of variants is judged from the
// variants as written, see writtenKey, without comparing them, so in time
// linear in the document, and breaks both unless it is one of these:
// variants only added to a list widen and only removed narrow, and a list
// that only repeats a variant more or fewer times breaks neither; variants
// given where there were none, with nothing beside them but annotations and
// nullable, in place of a schema written as one of them widen, and dropped
// back to such a schema narrow; variants given beside other keywords narrow
// and dropped from beside them widen; anyOf turned into oneOf with the same
// variants narrows and the reverse widens. The models that the variants
// kept, written the same on both sides, refer to are recorded as used
// there, so their changes reach the use; a variant added or removed is
// judged by the variants change alone. What it records beside the
// comparison of lists by position is done in functions of its own, so each
// level of a deep comparison takes little stack.
func (w *walk) variants(b, h, bf, hf *Schema, path *diffPath, rec bool) bool {
	if w.keywordChanged(bf, hf, path) {
		return true
	}
	changed := false
	for _, label := range [2]string{"one of", "any of"} {
		bl := variantsOf(bf, label)
		hl := variantsOf(hf, label)
		if len(bl) != len(hl) {
			w.listChanged(label, b, h, bf, hf, bl, hl, path)
			changed = true
			continue
		}
		for i := range bl {
			res := w.schema(bl[i], hl[i], extend(path, label+" #"+strconv.Itoa(i+1)), rec)
			if res.head || res.enum || res.inside {
				changed = true
			}
		}
	}
	return changed
}

// variantsOf returns the variants of f under the keyword named by label.
func variantsOf(f *Schema, label string) []*Schema {
	if label == "one of" {
		return f.OneOf
	}
	return f.AnyOf
}

// keywordChanged records a change where one of the flattened bf and hf has
// only oneOf variants and the other only anyOf ones, see variants, and
// reports whether there is one.
func (w *walk) keywordChanged(bf, hf *Schema, path *diffPath) bool {
	onlyOne := func(f *Schema) bool {
		return len(f.OneOf) > 0 && len(f.AnyOf) == 0
	}
	onlyAny := func(f *Schema) bool {
		return len(f.AnyOf) > 0 && len(f.OneOf) == 0
	}
	if onlyOne(bf) && onlyAny(hf) || onlyAny(bf) && onlyOne(hf) {
		bl, bLabel := variantList(bf)
		hl, hLabel := variantList(hf)
		w.keptUses(bl, hl, path)
		narrows := true
		widens := true
		if w.d.subsetWritten(bl, hl) && w.d.subsetWritten(hl, bl) {
			narrows = bLabel == "any of"
			widens = !narrows
		}
		w.add(Changed, path, bLabel+" variants changed to "+hLabel, narrows, widens)
		return true
	}
	return false
}

// listChanged records the change of the variants bl and hl of one keyword,
// named by label, lists of different lengths, of the flattened bf and hf,
// written as b and h, see variants.
func (w *walk) listChanged(label string, b, h, bf, hf *Schema, bl, hl []*Schema, path *diffPath) {
	switch {
	case len(bl) == 0:
		plain := []*Schema{withoutNull(b)}
		w.keptUses(plain, hl, path)
		narrows := true
		widens := false
		if replaces(hf, label) {
			narrows = !w.d.subsetWritten(plain, hl)
			widens = true
		}
		w.add(Changed, path, label+" variants given", narrows, widens)
	case len(hl) == 0:
		plain := []*Schema{withoutNull(h)}
		w.keptUses(bl, plain, path)
		narrows := false
		widens := true
		if replaces(bf, label) {
			narrows = true
			widens = !w.d.subsetWritten(plain, bl)
		}
		w.add(Changed, path, label+" variants dropped", narrows, widens)
	default:
		w.keptUses(bl, hl, path)
		added := w.d.subsetWritten(bl, hl)
		removed := w.d.subsetWritten(hl, bl)
		detail := label + " variants changed"
		switch {
		case added && removed:
			// The same variants, one of them listed more or fewer times.
			w.add(Changed, path, detail, false, false)
			return
		case added:
			detail = label + " variants added"
		case removed:
			detail = label + " variants removed"
		}
		w.add(Changed, path, detail, !added, !removed)
	}
}

// folded reports whether s is what Parse made of a oneOf or anyOf of one
// schema and {type: null}: that schema, as a single-member allOf or, for a
// bare $ref, as the $ref, made nullable, with no variants left, see
// normalizeNullMembers. A schema written as a single-member allOf, or a
// $ref, beside a oneOf or anyOf of only {type: null}, which Parse folds the
// same way, is one too.
func folded(s *Schema) bool {
	if s == nil || !s.NullMember || len(s.OneOf) > 0 || len(s.AnyOf) > 0 {
		return false
	}
	return len(s.AllOf) == 1 && s.Ref == "" || s.Ref != "" && len(s.AllOf) == 0
}

// unfold returns the folded s, see folded, as the list of its one variant,
// nullable, under the keyword of the variants the other side, other, lists,
// its anyOf where it has both, which Parse did not keep. The result
// is kept per schema and keyword, so the flattening and the cycle guard,
// keyed by schema, see the same one each time.
func (d *differ) unfold(s, other *Schema) *Schema {
	key := unfoldKey{s: s, any: len(other.OneOf) == 0 || len(other.AnyOf) > 0}
	out, ok := d.unfolded[key]
	if ok {
		return out
	}
	u := *s
	member := &Schema{Ref: s.Ref}
	if s.Ref == "" {
		member = s.AllOf[0]
	}
	u.Ref = ""
	u.AllOf = nil
	if key.any {
		u.AnyOf = []*Schema{member}
	} else {
		u.OneOf = []*Schema{member}
	}
	d.unfolded[key] = &u
	return &u
}

// unfoldKey names an unfolded schema: the folded one, and whether it is
// unfolded under anyOf rather than oneOf.
type unfoldKey struct {
	s   *Schema
	any bool
}

// variantList returns the variants of f, its oneOf or else its anyOf, and
// the label of that keyword.
func variantList(f *Schema) ([]*Schema, string) {
	if len(f.OneOf) > 0 {
		return f.OneOf, "one of"
	}
	return f.AnyOf, "any of"
}

// replaces reports whether the flattened f holds nothing beside its list of
// variants named by label but annotations and nullable, so it is that list
// and no more, give or take null, which a made nullable or no longer
// nullable change reports on its own.
func replaces(f *Schema, label string) bool {
	rest := *withoutNull(f)
	if label == "one of" {
		rest.OneOf = nil
	} else {
		rest.AnyOf = nil
	}
	return allowsAll(&rest)
}

// withoutNull returns s without nullable, and without the note that a
// {type: null} variant was folded into it, see normalizeNullMembers.
func withoutNull(s *Schema) *Schema {
	if !s.Nullable && !s.NullMember {
		return s
	}
	out := *s
	out.Nullable = false
	out.NullMember = false
	return &out
}

// keptUses records as used at path the models that the variants of a
// written the same as a variant of b refer to, see namedUses: the
// variants kept on both sides. A variant on one side only is judged by the
// change of variants, as a client could not send or receive it there
// before, or no longer can.
func (w *walk) keptUses(a, b []*Schema, path *diffPath) {
	keys := map[string]bool{}
	for _, s := range b {
		keys[w.d.key(s)] = true
	}
	for _, s := range a {
		if keys[w.d.key(s)] {
			w.namedUses(s, path, false, false)
		}
	}
}

// namedUses records the models s refers to, as written, as used at path: a
// $ref anywhere inside it names one, with how the nots between s and the
// $ref stand, see refUse, starting from not and strict, which are set for
// the schema under a not. References are not followed, so this ends on any
// document.
func (w *walk) namedUses(s *Schema, path *diffPath, not, strict bool) {
	if s == nil {
		return
	}
	name := RefName(s.Ref, refSchemas)
	if name != "" {
		w.uses = append(w.uses, refUse{name: name, path: path, ctx: w.ctx, not: not, strict: strict})
	}
	for _, key := range s.Properties.Keys {
		w.namedUses(s.Properties.Map[key], path, not, strict)
	}
	w.namedUses(s.Items, path, not, strict)
	w.namedUses(s.Not, path, !not, true)
	w.namedUses(s.AdditionalSchema(), path, not, strict)
	for _, m := range slices.Concat(s.AllOf, s.OneOf, s.AnyOf) {
		w.namedUses(m, path, not, strict)
	}
}

// mergedUses records as used at path, quietly, see refUse, the models that
// h, compared in place, refers to where flattening merges them into it: its
// $ref and those of its allOf members, at any depth, so their changes, which
// the comparison reports itself, are judged where h is used. References are
// not followed, so this ends on any document.
func (w *walk) mergedUses(h *Schema, path *diffPath) {
	key := quietKey{s: h, ctx: w.ctx}
	if h.Ref == "" && len(h.AllOf) == 0 || w.quiet[key] {
		return
	}
	if w.quiet == nil {
		w.quiet = map[quietKey]bool{}
	}
	w.quiet[key] = true
	name := RefName(h.Ref, refSchemas)
	if name != "" {
		w.quietUse(name, path)
	}
	for _, m := range h.AllOf {
		if m != nil {
			w.mergedUses(m, path)
		}
	}
}

// quietUse records a quiet use of the model name at path, see refUse, once
// per context: a quiet use gives no entry, so only its context counts.
func (w *walk) quietUse(name string, path *diffPath) {
	key := quietKey{name: name, ctx: w.ctx}
	if w.quiet[key] {
		return
	}
	if w.quiet == nil {
		w.quiet = map[quietKey]bool{}
	}
	w.quiet[key] = true
	w.uses = append(w.uses, refUse{name: name, path: path, ctx: w.ctx, quiet: true})
}

// quietKey is a model, by name, or a schema, in a context, see walk.quiet.
type quietKey struct {
	name string
	s    *Schema
	ctx  diffCtx
}

// subsetWritten reports whether every variant of a is written the same as
// some variant of b, see writtenKey.
func (d *differ) subsetWritten(a, b []*Schema) bool {
	keys := map[string]bool{}
	for _, s := range b {
		keys[d.key(s)] = true
	}
	for _, s := range a {
		if !keys[d.key(s)] {
			return false
		}
	}
	return true
}

// key is writtenKey, kept per schema.
func (d *differ) key(s *Schema) string {
	k, ok := d.keys[s]
	if !ok {
		k = writtenKey(s)
		d.keys[s] = k
	}
	return k
}

// writtenKey is the schema s as written, without its annotations (title,
// description, default, example, examples, deprecated, readOnly and
// writeOnly, at any depth, see writtenFields) and without following
// references, so two schemas have the same key when they say the same thing
// in the same words, a $ref the same when it names the same component.
// Properties are written in the order of their names, and required names,
// types and enum values as sets, see writeSet, as their order says nothing.
func writtenKey(s *Schema) string {
	var sb strings.Builder
	writeSchema(&sb, s, writtenFields)
	return sb.String()
}

// annotatedKey is writtenKey with the annotations written too, so two
// schemas have the same one where they are written the same but for their
// extensions.
func annotatedKey(s *Schema) string {
	var sb strings.Builder
	writeSchema(&sb, s, annotatedFields)
	return sb.String()
}

// writtenFields holds the index of each field of Schema that writtenKey
// writes: all but the annotations and the extensions.
var writtenFields = schemaFieldsBut("Title", "Description", "Default", "Example", "Examples", "Deprecated", "ReadOnly", "WriteOnly", "Extensions")

// annotatedFields holds the index of each field of Schema that annotatedKey
// writes: all but the extensions.
var annotatedFields = schemaFieldsBut("Extensions")

// schemaFieldsBut returns the index of each field of Schema but those named.
func schemaFieldsBut(names ...string) []int {
	var out []int
	t := reflect.TypeOf(Schema{})
	for i := range t.NumField() {
		if !slices.Contains(names, t.Field(i).Name) {
			out = append(out, i)
		}
	}
	return out
}

// writeSchema writes the key of s, see writtenKey: each of fields it sets,
// by index, and the schemas under it the same way.
func writeSchema(sb *strings.Builder, s *Schema, fields []int) {
	if s == nil {
		sb.WriteByte('-')
		return
	}
	sb.WriteByte('{')
	v := reflect.ValueOf(s).Elem()
	for _, i := range fields {
		f := v.Field(i)
		if f.IsZero() {
			continue
		}
		sb.WriteString(strconv.Itoa(i))
		sb.WriteByte(':')
		switch x := f.Interface().(type) {
		case *Schema:
			writeSchema(sb, x, fields)
		case []*Schema:
			sb.WriteByte('[')
			for _, m := range x {
				writeSchema(sb, m, fields)
				sb.WriteByte(',')
			}
			sb.WriteByte(']')
		case Ordered[*Schema]:
			sb.WriteByte('[')
			for _, key := range slices.Sorted(slices.Values(x.Keys)) {
				sb.WriteString(strconv.Quote(key))
				writeSchema(sb, x.Map[key], fields)
				sb.WriteByte(',')
			}
			sb.WriteByte(']')
		case []string:
			writeSet(sb, x)
		case TypeList:
			writeSet(sb, x)
		case []any:
			if i != enumField {
				fmt.Fprintf(sb, "%#v", x)
				break
			}
			values := make([]string, len(x))
			for j, v := range x {
				values[j] = fmt.Sprintf("%#v", v)
			}
			writeSet(sb, values)
		case *AdditionalProperties:
			sb.WriteString(strconv.FormatBool(x.Allowed))
			writeSchema(sb, x.Schema, fields)
		case *Bound:
			sb.WriteString(strconv.FormatBool(x.Flag))
			if x.Value != nil {
				sb.WriteString(strconv.FormatFloat(*x.Value, 'g', -1, 64))
			}
		case *float64:
			sb.WriteString(strconv.FormatFloat(*x, 'g', -1, 64))
		case *int:
			sb.WriteString(strconv.Itoa(*x))
		case *Discriminator:
			fmt.Fprintf(sb, "%q%#v", x.PropertyName, x.Mapping)
		default:
			fmt.Fprintf(sb, "%#v", x)
		}
		sb.WriteByte(';')
	}
	sb.WriteByte('}')
}

// enumField is the index of the Enum field of Schema, whose values
// writeSchema writes as a set, unlike the examples.
var enumField = func() int {
	field, _ := reflect.TypeOf(Schema{}).FieldByName("Enum")
	return field.Index[0]
}()

// writeSet writes items as a set: sorted, each once, so lists that only
// order or repeat the same items differently, as required names, types and
// enum values may, are written the same.
func writeSet(sb *strings.Builder, items []string) {
	sb.WriteByte('[')
	for _, item := range slices.Compact(slices.Sorted(slices.Values(items))) {
		sb.WriteString(strconv.Quote(item))
		sb.WriteByte(',')
	}
	sb.WriteByte(']')
}

// typeText is the non-null type list of f, inferring object from
// properties or from additionalProperties, and array from items, when no
// type is declared. additionalProperties absent, true or {}, and items
// absent or {}, which all allow anything, infer nothing, so the spellings
// of one schema read the same.
func typeText(f *Schema) string {
	var types []string
	for _, t := range f.Type {
		if t != "null" {
			types = append(types, t)
		}
	}
	if len(types) == 0 {
		switch {
		case f.Properties.Len() > 0 || closed(f) || !allowsAll(f.AdditionalSchema()):
			return "object"
		case !allowsAll(f.Items):
			return "array"
		}
		return ""
	}
	return strings.Join(types, " | ")
}

// hasVariants reports whether f has oneOf or anyOf variants.
func hasVariants(f *Schema) bool {
	return len(f.OneOf) > 0 || len(f.AnyOf) > 0
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
		sb.WriteString("\n### " + markdownText(g.name) + "\n\n")
		for _, c := range g.changes {
			sb.WriteString("- " + markdownChange(c) + "\n")
		}
	}
}

// markdownChange writes a change as `GET /x` › response 200 › id: detail.
// The operation or model, the path segments and the detail come from the
// spec, which can hold any character, so the first segment is a code span,
// model names in the detail are code spans, and the rest is markdownText.
func markdownChange(c Change) string {
	segs := strings.Split(c.Path, pathSep)
	var sb strings.Builder
	sb.WriteString(codeSpan(segs[0]))
	for _, seg := range segs[1:] {
		sb.WriteString(pathSep)
		sb.WriteString(markdownText(seg))
	}
	sb.WriteString(": ")
	sb.WriteString(markdownDetail(c))
	return sb.String()
}

// markdownDetail writes the detail of c from its parts, when it has them,
// with the model names as code spans and the text between them as
// markdownText, and otherwise as markdownText.
func markdownDetail(c Change) string {
	if len(c.parts) == 0 {
		return markdownText(c.Detail)
	}
	var sb strings.Builder
	for i, part := range c.parts {
		if i%2 == 1 {
			sb.WriteString(codeSpan(part))
		} else {
			sb.WriteString(markdownText(part))
		}
	}
	return sb.String()
}

// markdownText makes spec text safe to write inline in markdown. Control
// characters, line breaks among them, become spaces, so the text stays on
// its line and cannot start a heading or a list item; the characters
// markdown gives a meaning are backslash-escaped, so they show as written
// and cannot open emphasis, a link, a table cell or raw HTML. Renderers
// that link bare URLs and email addresses, such as GFM's autolink extension
// and goldmark's Linkify, would link them, so the colon of ://, the dot
// after www and every @ are escaped too. GitHub, where a report may be
// posted, finds mentions (@name), email addresses and issue references
// (#123, GH-123) in the text as rendered, after the escapes are gone, so a
// zero-width space, U+200B, follows every @, and every # or GH- before a
// digit. The text then reads as written and mentions no account and
// references no issue there. GitHub still links what it finds by its other
// rules, a commit hash it knows and a reference with a prefix the
// repository set up as a custom autolink, such as TICKET-123, and renders
// an emoji shortcode such as :+1: as the emoji. Code spans, the model names
// of a detail, get no zero-width space, as GitHub neither mentions nor
// links inside them. One who copies the text from the report copies the
// zero-width spaces too.
func markdownText(s string) string {
	var sb strings.Builder
	for i, r := range s {
		switch {
		case unicode.IsControl(r):
			sb.WriteByte(' ')
		case strings.ContainsRune("\\`*_[]<>|#~&@", r),
			r == ':' && strings.HasPrefix(s[i+1:], "//"),
			r == '.' && strings.HasSuffix(s[:i], "www"):
			sb.WriteByte('\\')
			sb.WriteRune(r)
		default:
			sb.WriteRune(r)
		}
		if linkMark(s, i, r) {
			sb.WriteString(zeroWidthSpace)
		}
	}
	return sb.String()
}

const zeroWidthSpace = "\u200b"

// linkMark reports whether r, at byte i of s, is where GitHub would start a
// mention, an email address or an issue reference: an @, or a # or the -
// of GH- before a digit.
func linkMark(s string, i int, r rune) bool {
	digit := i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9'
	switch r {
	case '@':
		return true
	case '#':
		return digit
	case '-':
		return digit && i >= 2 && strings.EqualFold(s[i-2:i], "gh")
	}
	return false
}

// codeSpan writes s as a markdown code span. Control characters become
// spaces and the span is fenced with one more backtick than the longest run
// in s, so nothing in s can end it or leave its line; s that starts or ends
// with a backtick is padded with a space on both sides, which markdown
// strips.
func codeSpan(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	longest := 0
	run := 0
	for _, r := range s {
		if r != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	fence := strings.Repeat("`", longest+1)
	return fence + s + fence
}
