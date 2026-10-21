package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// scriptCall is one call the fake caller received.
type scriptCall struct {
	Name string
	Args map[string]any
}

// fakeScriptCaller answers from a fixed table of responses and errors and
// records every call.
type fakeScriptCaller struct {
	mu        sync.Mutex
	responses map[string]string
	errors    map[string]error
	calls     []scriptCall
}

func (f *fakeScriptCaller) Call(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, scriptCall{Name: name, Args: args})
	err := f.errors[name]
	if err != nil {
		return nil, err
	}
	return json.RawMessage(f.responses[name]), nil
}

// testScriptEngine is the engine of the process, which TestMain builds
// before any test starts its clock: compiling the module takes about 3 s
// under the race detector.
var testScriptEngine = sharedScriptEngine

func TestMain(m *testing.M) {
	_, err := testScriptEngine()
	if err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// runScript runs code on the shared engine without an error reporter.
func runScript(ctx context.Context, cat *Catalog, call Caller, cfg ScriptConfig, code string) (string, error) {
	engine, err := testScriptEngine()
	if err != nil {
		return "", err
	}
	return engine.run(ctx, cat, call, cfg, code, nil)
}

func scriptTestCatalog() *Catalog {
	listUnits := &Function{Name: "units.list_units", Module: "units", ReadOnly: true}
	getUnit := &Function{Name: "units.get_unit", Module: "units", ReadOnly: true}
	registerPackage := &Function{Name: "packages.register_package", Module: "packages"}
	units := &Module{Name: "units", Functions: []*Function{getUnit, listUnits}}
	packages := &Module{Name: "packages", Functions: []*Function{registerPackage}}
	return &Catalog{
		Modules: []*Module{packages, units},
		functions: map[string]*Function{
			listUnits.Name:       listUnits,
			getUnit.Name:         getUnit,
			registerPackage.Name: registerPackage,
		},
		modules: map[string]*Module{
			units.Name:    units,
			packages.Name: packages,
		},
	}
}

func newFakeScriptCaller() *fakeScriptCaller {
	return &fakeScriptCaller{
		responses: map[string]string{
			"units.list_units":          `{"items":[{"id":"u1","name":"Alpha"},{"id":"u2","name":"Beta"}],"total":2}`,
			"units.get_unit":            `{"id":"u1","name":"Alpha"}`,
			"packages.register_package": `{"id":"p1"}`,
		},
		errors: map[string]error{},
	}
}

func TestScriptChainsCalls(t *testing.T) {
	caller := newFakeScriptCaller()
	code := `const u = api.units.list_units({limit: 2})
const first = api.units.get_unit({id: u.items[0].id})
return u.items.map(i => i.id).concat([first.name])`

	out, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, code)
	require.NoError(t, err)
	assert.Equal(t, `["u1","u2","Alpha"]`, out)
	require.Len(t, caller.calls, 2)
	assert.Equal(t, "units.list_units", caller.calls[0].Name)
	assert.Equal(t, "units.get_unit", caller.calls[1].Name)
}

func TestScriptForwardsArgs(t *testing.T) {
	caller := newFakeScriptCaller()
	code := `api.units.list_units({limit: 2, status: "active", tags: ["a", "b"], filter: {open: true}, ratio: 0.5})
api.units.list_units()
api.units.list_units(null)
return null`

	_, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, code)
	require.NoError(t, err)
	require.Len(t, caller.calls, 3)
	want := map[string]any{
		"limit":  float64(2),
		"status": "active",
		"tags":   []any{"a", "b"},
		"filter": map[string]any{"open": true},
		"ratio":  0.5,
	}
	assert.Equal(t, want, caller.calls[0].Args)
	assert.Equal(t, map[string]any{}, caller.calls[1].Args)
	assert.Equal(t, map[string]any{}, caller.calls[2].Args)
}

func TestScriptRejectsNonObjectArgs(t *testing.T) {
	caller := newFakeScriptCaller()

	_, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, `api.units.list_units(5)`)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "units.list_units: arguments must be an object")
	assert.Empty(t, caller.calls)
}

func TestScriptCallerErrorIsCatchable(t *testing.T) {
	caller := newFakeScriptCaller()
	caller.errors["units.get_unit"] = ez.New(ez.ENOTFOUND, "unit u9 not found", nil)
	code := `try {
  api.units.get_unit({id: "u9"})
  return "no error"
} catch (e) {
  return {message: e.message, code: e.code, isError: e instanceof Error}
}`

	out, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, code)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.True(t, strings.HasPrefix(got["message"].(string), "units.get_unit: not_found: "), got["message"])
	assert.Contains(t, got["message"], "unit u9 not found")
	assert.Equal(t, ez.ENOTFOUND, got["code"])
	assert.Equal(t, true, got["isError"])
}

func TestScriptUncaughtCallerError(t *testing.T) {
	caller := newFakeScriptCaller()
	caller.errors["units.get_unit"] = ez.New(ez.ENOTFOUND, "unit u9 not found", nil)
	code := `console.log("before")
api.units.get_unit({id: "u9"})
return 1`

	out, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, code)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "mcp: script error: Error: units.get_unit: not_found: ")
	assert.Contains(t, err.Error(), "(line 2)")
	assert.Equal(t, "log: before", out)
}

func TestScriptReadOnlyGuard(t *testing.T) {
	caller := newFakeScriptCaller()
	code := `try {
  api.packages.register_package({name: "x"})
} catch (e) {
  return e.message
}`

	out, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, code)
	require.NoError(t, err)
	assert.Equal(t, `"packages.register_package is not read-only; run_script only allows read-only functions on this server"`, out)
	assert.Empty(t, caller.calls)

	_, err = runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, `api.packages.register_package({name: "x"})`)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "packages.register_package is not read-only")
}

func TestScriptAllowWrites(t *testing.T) {
	caller := newFakeScriptCaller()

	out, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{AllowWrites: true}, `return api.packages.register_package({name: "x"}).id`)
	require.NoError(t, err)
	assert.Equal(t, `"p1"`, out)
	require.Len(t, caller.calls, 1)
	assert.Equal(t, scriptCall{Name: "packages.register_package", Args: map[string]any{"name": "x"}}, caller.calls[0])
}

func TestScriptMaxCalls(t *testing.T) {
	caller := newFakeScriptCaller()
	code := `let done = 0
try {
  for (let i = 0; i < 10; i++) {
    api.units.list_units()
    done++
  }
} catch (e) {
  return {done: done, message: e.message}
}`

	out, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{MaxCalls: 3}, code)
	require.NoError(t, err)
	assert.JSONEq(t, `{"done":3,"message":"script exceeded 3 function calls"}`, out)
	assert.Len(t, caller.calls, 3)
}

func TestScriptTimeout(t *testing.T) {
	start := time.Now()
	_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 50 * time.Millisecond}, `while (true) {}`)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "timed out after 50ms")
	assert.Less(t, elapsed, 500*time.Millisecond)
}

func TestScriptTimeoutNotCatchable(t *testing.T) {
	code := `try { while (true) {} } catch (e) { return "caught" }`

	_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 50 * time.Millisecond}, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestScriptContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()

	_, err := runScript(ctx, scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 10 * time.Second}, `while (true) {}`)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Equal(t, ez.ECANCELED, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "script canceled")
	assert.Less(t, elapsed, 2*time.Second)
}

func TestScriptConsoleLog(t *testing.T) {
	code := `console.log("first", 1, {a: [1, 2]}, null, undefined, true)
const u = api.units.list_units()
console.log("total", u.total)
try { api.packages.register_package() } catch (e) { console.log(e) }
return "done"`

	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, code)
	require.NoError(t, err)
	want := strings.Join([]string{
		`"done"`,
		`log: first 1 {"a":[1,2]} null undefined true`,
		`log: total 2`,
		`log: Error: packages.register_package is not read-only; run_script only allows read-only functions on this server`,
	}, "\n")
	assert.Equal(t, want, out)
}

func TestScriptReturnValues(t *testing.T) {
	cases := map[string]string{
		`return {id: "u1", count: 2, nested: {ok: true}, list: [1, "a"]}`: `{"id":"u1","count":2,"nested":{"ok":true},"list":[1,"a"]}`,
		`const x = 1`:                           `null`,
		`return`:                                `null`,
		`return null`:                           `null`,
		`return undefined`:                      `null`,
		`return function() {}`:                  `null`,
		`return api.units.get_unit({id: "u1"})`: `{"id":"u1","name":"Alpha"}`,
		`return api.units.list_units()`:         `{"items":[{"id":"u1","name":"Alpha"},{"id":"u2","name":"Beta"}],"total":2}`,
		`return "text"`:                         `"text"`,
		`return {toJSON() { return 7 }}`:        `7`,
	}
	for code, want := range cases {
		out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, code)
		require.NoError(t, err, code)
		assert.Equal(t, want, out, code)
	}
}

func TestScriptSyntaxError(t *testing.T) {
	_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `return {`)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "script error")
	assert.Contains(t, err.Error(), "SyntaxError")
}

func TestScriptThrownError(t *testing.T) {
	code := `const a = 1
throw new Error("boom")`

	_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, code)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "mcp: script error: Error: boom (line 2)")

	// Errors raised by builtins and the api name the agent's line.
	for _, call := range []string{"JSON.parse('{')", "null.x", "api.packages.register_package({})", "Array.prototype.join.call(null)"} {
		_, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, "const x = 1\nconst y = 2\n"+call)
		require.Error(t, err, call)
		assert.Contains(t, err.Error(), "(line 3)", call)
	}

	// Values that are not errors are described without running their
	// methods, and values the runtime cannot convert still give a text.
	cases := map[string]string{
		`throw null`:                     "mcp: script error: null",
		`throw undefined`:                "mcp: script error: undefined",
		`throw "text"`:                   "mcp: script error: text",
		`throw 5`:                        "mcp: script error: 5",
		`throw Symbol("s")`:              "mcp: script error: Symbol(s)",
		`throw []`:                       "mcp: script error: [object Array]",
		`throw {code: "x"}`:              "mcp: script error: [object Object]",
		`throw {toString() { throw 1 }}`: "mcp: script error: [object Object]",
		`const e = new Error("a"); Object.defineProperty(e, "stack", {value: {toString() { throw null }}}); throw e`: "mcp: script error: Error: a",
		`throw {get [Symbol.toStringTag]() { throw null }}`:                                                          "mcp: script error: a thrown value that cannot be converted to text",
		`return {get x() { throw new Error("boom") }}`:                                                               "mcp: script error: Error: boom (line 1)",
		`return {toJSON() { throw new Error("json") }}`:                                                              "mcp: script error: Error: json (line 1)",
		`api.units.list_units({get x() { throw new Error("arg") }})`:                                                 "mcp: script error: Error: arg (line 1)",
		"const a = {}\na.self = a\nreturn a":                                                                         "mcp: script error: TypeError",
	}
	for code, want := range cases {
		_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, code)
		require.Error(t, err, code)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), code)
		assert.Contains(t, err.Error(), want, code)
	}
}

func TestScriptNoExtraGlobals(t *testing.T) {
	code := `return [typeof require, typeof fetch, typeof setTimeout, typeof process, typeof std, typeof os, typeof print, typeof __call, typeof __log, typeof bjson, typeof scriptArgs, typeof gc, typeof api, typeof console.log, typeof Reflect.get]`

	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, code)
	require.NoError(t, err)
	assert.Equal(t, `["undefined","undefined","undefined","undefined","undefined","undefined","undefined","undefined","undefined","undefined","undefined","undefined","object","function","function"]`, out)
}

// TestScriptNoFilesystem: the runtime has no qjs:std, qjs:os or qjs:bjson
// module and no module loader, so every import fails.
func TestScriptNoFilesystem(t *testing.T) {
	for _, name := range []string{"qjs:std", "qjs:os", "qjs:bjson", "/etc/passwd", "lib.js"} {
		code := `import("` + name + `").then(m => console.log("loaded", Object.keys(m).join()), e => console.log(String(e)))
return 1`
		out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, code)
		require.NoError(t, err, name)
		assert.Equal(t, "1\nlog: ReferenceError: could not load module '"+name+"'", out, name)
	}
}

// TestScriptWaitIsRefused: there are no timers or sleep to wait on, and a
// promise that never settles does not hold the run open.
func TestScriptWaitIsRefused(t *testing.T) {
	for _, code := range []string{
		`(async () => { await new Promise(() => {}) })()
return 1`,
		`new Promise(() => {}).then(() => console.log("never"))
return 1`,
	} {
		start := time.Now()
		out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 2 * time.Second}, code)
		require.NoError(t, err, code)
		assert.Equal(t, "1", out, code)
		assert.Less(t, time.Since(start), time.Second, code)
	}
}

func TestScriptDescription(t *testing.T) {
	text := scriptDescription(ScriptConfig{Enabled: true})
	assert.Contains(t, text, "5s timeout, 200 function calls, read-only functions only")
	assert.Contains(t, text, "api.<module>.<function>({...})")
	assert.Contains(t, text, "search_functions")
	assert.Contains(t, text, "describe_function")
	assert.NotContains(t, text, "%")
	assert.Less(t, len(strings.Fields(text)), 120)

	text = scriptDescription(ScriptConfig{Enabled: true, AllowWrites: true, Timeout: 2 * time.Second, MaxCalls: 10})
	assert.Contains(t, text, "2s timeout, 10 function calls, functions that write are allowed")
}

func TestScriptCodeTooLarge(t *testing.T) {
	caller := newFakeScriptCaller()
	code := "api.units.list_units()\nreturn 1\n//"
	code += strings.Repeat("x", maxScriptBytes-len(code))

	out, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, code)
	require.NoError(t, err)
	assert.Equal(t, "1", out)
	require.Len(t, caller.calls, 1)

	_, err = runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, code+"x")
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "mcp: script is 65537 bytes; the limit is 65536")
	assert.Len(t, caller.calls, 1)
}

func TestScriptLogCap(t *testing.T) {
	code := `for (let i = 0; i < 300; i++) { console.log("line", i) }
return "done"`
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, code)
	require.NoError(t, err)
	lines := strings.Split(out, "\n")
	require.Len(t, lines, maxScriptLogLines+2)
	assert.Equal(t, `"done"`, lines[0])
	assert.Equal(t, "log: line 0", lines[1])
	assert.Equal(t, "log: line 199", lines[200])
	assert.Equal(t, "log: [truncated]", lines[201])

	// Each line is 10005 bytes, so the seventh passes 64 KB: what fits of
	// it is kept, then logging stops.
	code = `const big = "x".repeat(10000)
for (let i = 0; i < 20; i++) { console.log(big) }
console.log("after")
throw new Error("stop")`
	out, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, code)
	require.Error(t, err)
	lines = strings.Split(out, "\n")
	require.Len(t, lines, 8)
	assert.Equal(t, "log: "+strings.Repeat("x", 10000), lines[5])
	assert.Equal(t, "log: "+strings.Repeat("x", maxScriptLogBytes-6*10005-5), lines[6])
	assert.Equal(t, "log: [truncated]", lines[7])
}

// TestScriptFillRunStaysOffHeap: a script that fills its memory touches
// the mapping, not the Go heap. Growing on the Go heap allocated 275 MiB
// per such run.
func TestScriptFillRunStaysOffHeap(t *testing.T) {
	mapping, err := newGuestMemory()
	if err != nil {
		t.Skip("no memory mapping on this platform")
	}
	mapping.release()
	allocated := func() uint64 {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.TotalAlloc
	}
	before := allocated()
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `const a = []
try { for (;;) { a.push(new ArrayBuffer(1 << 20)) } } catch (e) {}
return a.length`)
	require.NoError(t, err)
	assert.NotEqual(t, "0", out)
	assert.Less(t, allocated()-before, uint64(2<<20), "allocated by a run that fills its memory")
}

// TestScriptOutOfMemoryIsNamed: when QuickJS cannot even allocate the
// out-of-memory error it throws null; the run still says out of memory.
func TestScriptOutOfMemoryIsNamed(t *testing.T) {
	for _, code := range []string{
		`const head = {}; let o = head; for (let i = 0; i < 1e8; i++) { o.n = {}; o = o.n }`,
		`const m = new Map(); for (let i = 0; i < 1e8; i++) { m.set(i, [i]) }`,
		`const a = []; for (;;) { a.push("x" + a.length) }`,
		`const a = []; for (let i = 0; ; i++) { a.push(String(i) + "y") }`,
		`const a = []; for (;;) { a.push([a.length]) }`,
		`const a = []; for (;;) { a.push(new Error("e")) }`,
		`const a = []; try { for (;;) { a.push([a.length]) } } catch (e) {} for (;;) { a.push("z" + a.length) }`,
	} {
		_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 30 * time.Second}, code)
		require.Error(t, err, code)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), code)
		assert.Contains(t, err.Error(), "out of memory", code)
		assert.NotContains(t, err.Error(), "null", code)
	}
}

// TestScriptOutOfMemoryIsCatchable: a second out-of-memory after the
// first was caught and memory freed is a real error too, because the
// allocator's grace returns once the heap is under the limit again.
func TestScriptOutOfMemoryIsCatchable(t *testing.T) {
	code := `const a = []
try { for (;;) { a.push([a.length]) } } catch (e) {}
a.length -= 10000
const b = []
try { for (;;) { b.push([b.length]) } } catch (e) { return "caught: " + e.message }`
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 30 * time.Second}, code)
	require.NoError(t, err)
	assert.Equal(t, `"caught: out of memory"`, out)

	// Freeing less than the grace is enough for the next error.
	code = strings.Replace(code, "a.length -= 10000", "a.length -= 50", 1)
	out, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 30 * time.Second}, code)
	require.NoError(t, err)
	assert.Equal(t, `"caught: out of memory"`, out)

	// Without frees, each refusal still gets a fresh grace, a few times.
	code = `const a = []
let seen = ""
for (let i = 0; i < 4; i++) {
	try { for (;;) { a.push({}) } } catch (e) { seen += (e === null ? "null" : e.message) + ";" }
}
return seen`
	out, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 30 * time.Second}, code)
	require.NoError(t, err)
	assert.Equal(t, `"out of memory;out of memory;out of memory;out of memory;"`, out)
}

// TestScriptLostWriteErrorIsFrozen: a script cannot change the error that
// stands in for a lost write reply, and a read-only call rethrows its own.
func TestScriptLostWriteErrorIsFrozen(t *testing.T) {
	caller := newFakeScriptCaller()
	caller.responses["packages.register_package"] = `{"blob":"` + strings.Repeat("x", 5<<20) + `"}`
	code := `try { api.packages.register_package({}) } catch (e) { try { e.message = "all fine" } catch (x) {} }
try { api.packages.register_package({}) } catch (e) { return e.message }`
	out, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{AllowWrites: true, MaxCalls: 2}, code)
	require.NoError(t, err)
	assert.Contains(t, out, "any changes it made stay")

	_, err = runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, `api.units.list_units({})
throw new Error("mine")`)
	require.Error(t, err)
	assert.Equal(t, "mcp: script error: Error: mine (line 2)", ez.ErrorMessage(err))

	// A call that fails before reaching the host is not a lost write: here
	// the arguments do not fit in memory, and the server refuses writes.
	code = `const a = []
try { for (;;) { a.push(new ArrayBuffer(1 << 20)) } } catch (e) {}
a.pop()
try { api.packages.register_package({blob: "x".repeat(1 << 21)}) } catch (e) { return String(e) }`
	caller = newFakeScriptCaller()
	out, err = runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{Timeout: 30 * time.Second}, code)
	require.NoError(t, err)
	assert.Contains(t, out, "out of memory")
	assert.NotContains(t, out, "may have been called")
	assert.Empty(t, caller.calls)
}

// TestScriptLogLinesAreOneLine: a logged value with line breaks cannot
// forge the lines of the tool result after it.
func TestScriptLogLinesAreOneLine(t *testing.T) {
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, "console.log(\"a\\nerror: fake\\r\\u0085\\v\\f\\x1b[31mb\"); return 1")
	require.NoError(t, err)
	assert.Equal(t, "1\nlog: a\\nerror: fake\\r\\u0085\\v\\f\\x1b[31mb", out)
}

// TestScriptLostWriteReply: a write whose response cannot be read by the
// script for lack of memory still tells the script it was called.
func TestScriptLostWriteReply(t *testing.T) {
	caller := newFakeScriptCaller()
	caller.responses["packages.register_package"] = `{"blob":"` + strings.Repeat("x", 2<<20) + `"}`
	code := `const a = []
try { for (;;) { a.push(new ArrayBuffer(1 << 20)) } } catch (e) {}
a.pop()
try { api.packages.register_package({}) } catch (e) { return String(e) }
return "read"`
	out, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{AllowWrites: true, Timeout: 30 * time.Second}, code)
	require.NoError(t, err)
	assert.Contains(t, out, "packages.register_package")
	assert.Contains(t, out, "any changes it made stay")
	assert.Len(t, caller.calls, 1)

	// A response Go refuses says the same.
	caller.responses["packages.register_package"] = `{"blob":"` + strings.Repeat("x", 5<<20) + `"}`
	out, err = runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{AllowWrites: true}, `try { api.packages.register_package({}) } catch (e) { return String(e) }`)
	require.NoError(t, err)
	assert.Contains(t, out, "packages.register_package ran, and any changes it made stay, but the response is")
}

// TestScriptErrorMessageIsOneLine: newlines in a thrown message cannot
// forge lines of the tool result under the error line.
func TestScriptErrorMessageIsOneLine(t *testing.T) {
	_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, "throw new Error(\"x\\nlog: fake\\r\\u2028\\u2029\\v\\f\\x1b[31my\")")
	require.Error(t, err)
	assert.Equal(t, `mcp: script error: Error: x\nlog: fake\r\u2028\u2029\v\f\x1b[31my (line 1)`, ez.ErrorMessage(err))
}

// TestScriptWritesListedOnFailure: a run that fails after calling a
// function that writes names it, ahead of the script's own message, so the
// agent does not repeat it.
func TestScriptWritesListedOnFailure(t *testing.T) {
	caller := newFakeScriptCaller()
	code := `api.packages.register_package({name: "x"})
api.packages.register_package({name: "y"})
throw new Error("boom")`
	_, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{AllowWrites: true}, code)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Equal(t, "mcp: script failed after calls that write, which may have changed data (packages.register_package): script error: Error: boom (line 3)", ez.ErrorMessage(err))
	assert.Len(t, caller.calls, 2)

	_, err = runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, `api.units.list_units()
throw new Error("boom")`)
	require.Error(t, err)
	assert.NotContains(t, ez.ErrorMessage(err), "write")
}

// TestScriptErrorMessageCapped: a thrown message is cut so the error of
// the run fits the output cap of the tool.
func TestScriptErrorMessageCapped(t *testing.T) {
	_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `throw new Error("m".repeat(30000))`)
	require.Error(t, err)
	message := ez.ErrorMessage(err)
	assert.Less(t, len(message), maxScriptErrorBytes+100)
	assert.Contains(t, message, " [truncated] (line 1)")
}

func TestScriptRecursionFailsFast(t *testing.T) {
	start := time.Now()
	_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 10 * time.Second}, `function f(n) { return f(n + 1) + 1 }
return f(0)`)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "mcp: script error: RangeError: Maximum call stack size exceeded (line 1)")
	assert.Less(t, time.Since(start), 2*time.Second)

	// Recursion within the stack limit, about 2900 frames of this
	// function, returns its value.
	for _, depth := range []string{"150", "1000", "2500"} {
		out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `function f(n) { return n === 0 ? 0 : f(n - 1) + 1 }
return f(`+depth+`)`)
		require.NoError(t, err, depth)
		assert.Equal(t, depth, out)
	}

	// Deeper recursion throws a RangeError the script can catch.
	_, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `function f(n) { return n === 0 ? 0 : f(n - 1) + 1 }
return f(10000)`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RangeError: Maximum call stack size exceeded (line 1)")
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `let depth = 0
function f() { depth++; f() }
try { f() } catch (e) { return depth > 2000 }`)
	require.NoError(t, err)
	assert.Equal(t, "true", out)

	// Retrying in every catch on the way down never ends; the timeout
	// stops it.
	start = time.Now()
	_, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 200 * time.Millisecond}, `function f(n) { try { return f(n + 1) + 1 } catch (e) { return f(n + 1) } }
return f(0)`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mcp: script timed out after 200ms")
	assert.Less(t, time.Since(start), 2*time.Second)

	// Without the QuickJS limit the recursion runs the stack below address
	// 0 and traps, and the next run starts clean.
	engine := uncheckedScriptEngine(t)
	for _, code := range []string{
		`function f(n) { return f(n + 1) + 1 }
return f(0)`,
		`let depth = 0
function f() { depth++; f() }
try { f() } catch (e) { return depth }`,
	} {
		start = time.Now()
		_, err = engine.run(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 10 * time.Second}, code, nil)
		require.Error(t, err, code)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), code)
		assert.Contains(t, err.Error(), "mcp: script error: the script exhausted the runtime stack or memory", code)
		assert.Less(t, time.Since(start), 2*time.Second, code)
	}
	out, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `return api.units.list_units().total`)
	require.NoError(t, err)
	assert.Equal(t, "2", out)
}

// uncheckedScriptEngine is the shared engine without the QuickJS stack and
// memory limits, so recursion traps and allocations reach the end of linear
// memory.
func uncheckedScriptEngine(t *testing.T) *scriptEngine {
	t.Helper()
	engine, err := testScriptEngine()
	require.NoError(t, err)
	unchecked := *engine
	unchecked.stackLimit = 0
	unchecked.memoryLimit = 0
	return &unchecked
}

// blockingCaller answers only when its context is done, like a slow REST
// handler that honors cancellation.
type blockingCaller struct{}

func (blockingCaller) Call(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestScriptTimeoutBoundsCalls(t *testing.T) {
	codes := []string{
		`return api.units.list_units()`,
		`try { api.units.list_units() } catch (e) { return "caught" }`,
	}
	for _, code := range codes {
		start := time.Now()
		_, err := runScript(context.Background(), scriptTestCatalog(), blockingCaller{}, ScriptConfig{Timeout: 50 * time.Millisecond}, code)
		require.Error(t, err, code)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), code)
		assert.Contains(t, err.Error(), "mcp: script timed out after 50ms", code)
		assert.Less(t, time.Since(start), time.Second, code)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := runScript(ctx, scriptTestCatalog(), blockingCaller{}, ScriptConfig{Timeout: 10 * time.Second}, `try { api.units.list_units() } catch (e) { return "caught" }`)
	require.Error(t, err)
	assert.Equal(t, ez.ECANCELED, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "mcp: script canceled: context canceled")
}

// TestScriptHostileProgramsAreContained: programs that overflowed the Go
// stack or pinned a core under goja end in an error or a result within the
// timeout, and the runtime keeps working afterwards.
func TestScriptHostileProgramsAreContained(t *testing.T) {
	programs := []string{
		`const a = {}; a.a = a; return JSON.stringify(a)`,
		`let a = []; for (let i = 0; i < 250000; i++) { a = [a] } return JSON.stringify(a)`,
		`let a = []; for (let i = 0; i < 250000; i++) { a = [a] } return a`,
		`let a = []; for (let i = 0; i < 250000; i++) { a = [a] } console.log(a); return 1`,
		`return JSON.parse("[".repeat(1e6))`,
		`let a = []; for (let i = 0; i < 250000; i++) { a = [a] } return a.flat(Infinity).length`,
		`let a = []; for (let i = 0; i < 250000; i++) { a = [a] } return String(a).length`,
		`const ap = Function.prototype.apply; return ap.apply(ap, [ap, [null, []]])`,
		`const a = [Reflect.apply, undefined, null]; a[2] = a; return Reflect.apply(Reflect.apply, undefined, a)`,
		`let p = {}; for (let i = 0; i < 1e6; i++) { p = new Proxy(p, {}) } return p.x`,
		`const a = []; a.length = 2 ** 32 - 1; return a.lastIndexOf(1)`,
		`return /^(a+)+$/.test("a".repeat(40) + "b")`,
		`function f(n) { return f(n + 1) + 1 } return f(0)`,
		`function f(n) { try { return f(n + 1) + 1 } catch (e) { return f(n + 1) } } return f(0)`,
		`let s = "x"; for (;;) { s += s }`,
		`const a = []; for (;;) { a.push(new Array(1e6).fill(1)) }`,
		`const a = []; for (let i = 0; ; i++) { a.push({k: i, v: "v" + i}) }`,
		`const a = []; for (;;) { a.push(new ArrayBuffer(1 << 20)) }`,
		`throw {toString() { for (;;) {} }}`,
		`return {get x() { for (;;) {} }}`,
		`return api.units.list_units({get x() { for (;;) {} }})`,
		`console.log({toJSON() { for (;;) {} }})`,
	}
	for _, code := range programs {
		start := time.Now()
		var err error
		require.NotPanics(t, func() {
			_, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: time.Second}, code)
		}, code)
		elapsed := time.Since(start)
		t.Logf("%s: %v after %s", code, err, elapsed.Round(time.Millisecond))
		assert.Less(t, elapsed, 3*time.Second, code)
		if err != nil {
			assert.Contains(t, []string{ez.EINVALID, ez.ECANCELED}, ez.ErrorCode(err), code+": "+err.Error())
		}
	}

	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `return api.units.get_unit({id: "u1"}).name`)
	require.NoError(t, err)
	assert.Equal(t, `"Alpha"`, out)
}

// TestScriptRepeatedTimeouts: runtimes stopped by the deadline do not break
// the ones after them.
func TestScriptRepeatedTimeouts(t *testing.T) {
	for i := 0; i < 10; i++ {
		_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 50 * time.Millisecond}, `for (;;) {}`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out after 50ms")
	}
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `return api.units.list_units().total`)
	require.NoError(t, err)
	assert.Equal(t, "2", out)
}

// TestScriptStateDoesNotLeak: every run starts from a fresh runtime.
func TestScriptStateDoesNotLeak(t *testing.T) {
	_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `globalThis.leak = 1
Object.prototype.polluted = 1
JSON.stringify = () => "hijacked"
api.units.list_units = () => "hijacked"
return 1`)
	require.NoError(t, err)

	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `return [typeof leak, typeof ({}).polluted, JSON.stringify(1), api.units.list_units().total]`)
	require.NoError(t, err)
	assert.Equal(t, `["undefined","undefined","1",2]`, out)
}

func TestScriptOversizedResponse(t *testing.T) {
	caller := newFakeScriptCaller()
	caller.responses["units.list_units"] = `"` + strings.Repeat("x", maxScriptResponseBytes-1) + `"`

	_, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, `return api.units.list_units()`)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "units.list_units: the response is 4194305 bytes; scripts accept at most 4194304")

	out, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, `try { api.units.list_units() } catch (e) { return "caught" }`)
	require.NoError(t, err)
	assert.Equal(t, `"caught"`, out)

	caller.responses["units.list_units"] = `{"items": [`
	_, err = runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, `return api.units.list_units()`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "units.list_units: the response is not JSON")
}

func TestScriptOversizedArguments(t *testing.T) {
	caller := newFakeScriptCaller()

	_, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, `return api.units.list_units({q: "x".repeat(1 << 20)})`)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "units.list_units: the arguments are 1048584 bytes of JSON; the limit is 1048576")
	assert.Empty(t, caller.calls)
}

func TestScriptOversizedResult(t *testing.T) {
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `return "x".repeat((1 << 20) - 2)`)
	require.NoError(t, err)
	assert.Len(t, out, maxScriptResultBytes)

	_, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `console.log("big")
return "x".repeat(1 << 20)`)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Equal(t, "mcp: script error: the result is 1048578 bytes of JSON; the limit is 1048576, return less", ez.ErrorMessage(err))
}

func TestScriptConcurrencyLimit(t *testing.T) {
	limiter := newScriptLimiter(1)

	releaseA, err := limiter.claim("a")
	require.NoError(t, err)

	// The same caller is refused at once; another caller is not.
	_, err = limiter.claim("a")
	require.Error(t, err)
	assert.Equal(t, ez.ERESOURCEEXHAUSTED, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "already running")
	releaseB, err := limiter.claim("b")
	require.NoError(t, err)

	releaseSlot, err := limiter.slot(context.Background())
	require.NoError(t, err)

	// With every slot busy, a script waits, then gives up.
	start := time.Now()
	_, err = limiter.slot(context.Background())
	require.Error(t, err)
	assert.Equal(t, ez.ERESOURCEEXHAUSTED, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "too many scripts")
	assert.GreaterOrEqual(t, time.Since(start), scriptSlotWait)

	// A slot freed while waiting is taken.
	time.AfterFunc(100*time.Millisecond, releaseSlot)
	releaseSlot, err = limiter.slot(context.Background())
	require.NoError(t, err)

	// A canceled caller stops waiting.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = limiter.slot(ctx)
	require.Error(t, err)
	assert.Equal(t, ez.ECANCELED, ez.ErrorCode(err))

	releaseSlot()
	releaseB()
	releaseA()
	releaseA, err = limiter.claim("a")
	require.NoError(t, err)
	releaseA()
	assert.Empty(t, limiter.running)
	assert.Empty(t, limiter.slots)

	assert.Equal(t, defaultScriptMaxConcurrent, cap(newScriptLimiter(0).slots))
}

// TestScriptWasmMemoryIsCapped: the embedded QuickJS binary is the output of
// quickjs/build.sh. It declares one memory with a maximum of 1024 pages
// (64 MiB), and its 1 MiB stack sits below every data segment, so a deep
// recursion traps below address 0 instead of overwriting static data.
func TestScriptWasmMemoryIsCapped(t *testing.T) {
	sum := sha256.Sum256(quickjsWasm)
	assert.Equal(t, "2940915363534833f5600c3a0859fd4fe89b71851e69cde4acb5df03ad4d7fda", hex.EncodeToString(sum[:]))

	data := quickjsWasm
	require.Equal(t, []byte("\x00asm\x01\x00\x00\x00"), data[:8])
	memories := 0
	stackPointer := int64(-1)
	dataStart := int64(-1)
	for i := 8; i < len(data); {
		id := data[i]
		size, n := binary.Uvarint(data[i+1:])
		require.Positive(t, n)
		body := data[i+1+n : i+1+n+int(size)]
		i += 1 + n + int(size)
		switch id {
		case 5:
			count, n := binary.Uvarint(body)
			require.Equal(t, uint64(1), count)
			body = body[n:]
			flags := body[0]
			require.Equal(t, byte(1), flags&1, "the memory must declare a maximum")
			_, n = binary.Uvarint(body[1:])
			max, _ := binary.Uvarint(body[1+n:])
			assert.Equal(t, uint64(1024), max)
			memories++
		case 6:
			// The first global is __stack_pointer: i32, mutable, i32.const.
			_, n := binary.Uvarint(body)
			body = body[n:]
			require.Equal(t, []byte{0x7f, 0x01, 0x41}, body[:3])
			stackPointer, _ = wasmSigned(body[3:])
		case 11:
			count, n := binary.Uvarint(body)
			body = body[n:]
			for j := uint64(0); j < count; j++ {
				require.Equal(t, byte(0), body[0], "active data segment")
				require.Equal(t, byte(0x41), body[1])
				offset, n := wasmSigned(body[2:])
				body = body[2+n:]
				require.Equal(t, byte(0x0b), body[0])
				length, n := binary.Uvarint(body[1:])
				body = body[1+n+int(length):]
				if dataStart < 0 || offset < dataStart {
					dataStart = offset
				}
			}
		}
	}
	assert.Equal(t, 1, memories)
	assert.Equal(t, int64(1<<20), stackPointer)
	assert.GreaterOrEqual(t, dataStart, stackPointer, "the stack must sit below the data")
}

// wasmSigned decodes a signed LEB128 number, the encoding of i32.const.
func wasmSigned(b []byte) (int64, int) {
	value := int64(0)
	shift := uint(0)
	for i, c := range b {
		value |= int64(c&0x7f) << shift
		shift += 7
		if c&0x80 == 0 {
			if shift < 64 && c&0x40 != 0 {
				value |= -1 << shift
			}
			return value, i + 1
		}
	}
	return 0, 0
}

// TestScriptRunsDoNotLeak: runs stopped by the deadline, runs that trap,
// runs that end normally and runs that fill linear memory leave neither heap
// nor goroutines behind.
func TestScriptRunsDoNotLeak(t *testing.T) {
	engine, err := testScriptEngine()
	require.NoError(t, err)
	// The guest memory is mapped outside the Go heap, so the heap does not
	// show a mapping that is never released; count them instead.
	maps := 0
	unmaps := 0
	unavailable := false
	mapGuestMemoryFunc = func(size int) ([]byte, error) {
		buf, err := mapGuestMemory(size)
		if err != nil {
			unavailable = ez.ErrorCode(err) == ez.EUNAVAILABLE
			return nil, err
		}
		maps++
		return buf, nil
	}
	unmapGuestMemoryFunc = func(buf []byte) {
		unmaps++
		unmapGuestMemory(buf)
	}
	defer func() {
		mapGuestMemoryFunc = mapGuestMemory
		unmapGuestMemoryFunc = unmapGuestMemory
	}()
	unchecked := uncheckedScriptEngine(t)
	cat := scriptTestCatalog()
	caller := newFakeScriptCaller()
	ctx := context.Background()
	series := []struct {
		engine  *scriptEngine
		runs    int
		timeout time.Duration
		code    string
	}{
		{engine, 100, 5 * time.Millisecond, `for (;;) {}`},
		{unchecked, 100, 10 * time.Second, `function f() { f() } f()`},
		{engine, 100, 10 * time.Second, `return api.units.list_units().total`},
		{engine, 10, 10 * time.Second, `const a = []; for (;;) { a.push(new ArrayBuffer(1 << 20)) }`},
	}
	runAll := func(runs int) {
		for _, s := range series {
			for i := 0; i < min(runs, s.runs); i++ {
				_, _ = s.engine.run(ctx, cat, caller, ScriptConfig{Timeout: s.timeout}, s.code, nil)
			}
			runtime.GC()
			runtime.GC()
		}
	}
	heap := func() uint64 {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.HeapAlloc
	}

	// One run of each kind first, so pools and lazy state exist.
	runAll(1)
	goroutines := runtime.NumGoroutine()
	before := heap()
	start := time.Now()
	runAll(100)
	after := heap()
	t.Logf("310 runs in %s, heap %d KiB before, %d KiB after", time.Since(start).Round(time.Millisecond), before>>10, after>>10)
	assert.Less(t, int64(after)-int64(before), int64(8<<20))
	// The goroutines that close an instance at its deadline end with the
	// call; give the last ones a moment.
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > goroutines && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.LessOrEqual(t, runtime.NumGoroutine(), goroutines)
	if !unavailable {
		assert.Equal(t, 314, maps)
		assert.Equal(t, maps, unmaps, "mappings released")
	}
}

// TestScriptCallVolume: replies are freed once the script has them, so the
// traffic of a run is not bounded by the guest's memory: 80 responses of
// 1 MiB pass through a 64 MiB guest. Each MiB costs about 25 ms of QuickJS
// time, 100 ms under -race, so the test stops there.
func TestScriptCallVolume(t *testing.T) {
	caller := newFakeScriptCaller()
	caller.responses["units.list_units"] = `"` + strings.Repeat("x", (1<<20)-2) + `"`
	code := `for (let i = 0; i < 80; i++) { api.units.list_units() }
return "ok"`

	out, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{Timeout: 30 * time.Second}, code)
	require.NoError(t, err)
	assert.Equal(t, `"ok"`, out)
	assert.Len(t, caller.calls, 80)
}

// panicCaller panics on every call.
type panicCaller struct{}

func (panicCaller) Call(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	panic("kaboom")
}

// TestScriptHostPanic: a panic in a function call reaches the script as a
// generic error and the reporter with its stack.
func TestScriptHostPanic(t *testing.T) {
	engine, err := testScriptEngine()
	require.NoError(t, err)
	var reported []error
	onError := func(ctx context.Context, err error) {
		reported = append(reported, err)
	}

	_, err = engine.run(context.Background(), scriptTestCatalog(), panicCaller{}, ScriptConfig{}, `return api.units.list_units()`, onError)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Equal(t, "mcp: script error: Error: units.list_units: the call failed (line 1)", ez.ErrorMessage(err))
	require.Len(t, reported, 1)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(reported[0]))
	assert.Contains(t, reported[0].Error(), "units.list_units panicked: kaboom")
	assert.Contains(t, reported[0].Error(), "goroutine ")
	assert.Contains(t, reported[0].Error(), "TestScriptHostPanic")

	out, err := engine.run(context.Background(), scriptTestCatalog(), panicCaller{}, ScriptConfig{}, `try { api.units.list_units() } catch (e) { return [e.message, e.stack.includes("goroutine")] }`, onError)
	require.NoError(t, err)
	assert.Equal(t, `["units.list_units: the call failed",false]`, out)
	assert.Len(t, reported, 2)
}

// TestScriptOutOfMemoryDuringCall: a reply that does not fit in the guest's
// memory becomes an error the script can catch; the call has already run.
func TestScriptOutOfMemoryDuringCall(t *testing.T) {
	engine := uncheckedScriptEngine(t)
	caller := newFakeScriptCaller()
	big := `"` + strings.Repeat("x", (4<<20)-10) + `"`
	caller.responses["units.list_units"] = big
	caller.responses["packages.register_package"] = big
	fill := `const a = []
try { for (;;) { a.push(new ArrayBuffer(1 << 20)) } } catch (e) {}
a.pop()
a.pop()
`

	out, err := engine.run(context.Background(), scriptTestCatalog(), caller, ScriptConfig{AllowWrites: true}, fill+`try { api.units.list_units() } catch (e) { return e.message }`, nil)
	require.NoError(t, err)
	assert.Equal(t, `"units.list_units: the script ran out of memory before the response could be delivered"`, out)

	out, err = engine.run(context.Background(), scriptTestCatalog(), caller, ScriptConfig{AllowWrites: true}, fill+`api.packages.register_package({})`, nil)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "packages.register_package ran, and any changes it made stay, but the script ran out of memory")
	assert.NotContains(t, err.Error(), "goroutine")
	assert.Empty(t, out)
	assert.Len(t, caller.calls, 2)

	// With the QuickJS limit the heap runs out first and the script sees
	// QuickJS's own error.
	_, err = runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, `const a = []
for (;;) { a.push(new ArrayBuffer(1 << 20)) }`)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "InternalError: out of memory")
}

// TestScriptCodeReachesOnlyGlobalScope: the code reaches new Function as a
// string and no Go-side rule lives in JavaScript around it. Text that closes
// the function body early is a syntax error or code at global scope, where
// the shim keeps nothing it depends on, and the result cap holds whatever
// the code returns.
func TestScriptCodeReachesOnlyGlobalScope(t *testing.T) {
	_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `})()); } catch (e) {} })(); (function () { return "R" + "x".repeat(5e6); try { const result = JSON.stringify((function () {`)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "SyntaxError")

	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `}); (function () { return "R" + "x".repeat(2)`)
	require.NoError(t, err)
	assert.Equal(t, `"Rxx"`, out)

	_, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `return "x".repeat(3 << 20)`)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Equal(t, "mcp: script error: the result is 3145730 bytes of JSON; the limit is 1048576, return less", ez.ErrorMessage(err))
}

// TestScriptLineNumbers: errors name the line of the agent's code, also for
// syntax errors.
func TestScriptLineNumbers(t *testing.T) {
	cases := map[string]string{
		"const a = 1\nthrow new Error(\"boom\")":          "mcp: script error: Error: boom (line 2)",
		"const a = 1\nconst b = 2\nconst = 3":             "(line 3)",
		"const a = 1\nconst b = 2\nJSON.parse('\\n\\n{')": "(line 3)",
		"\n\n\n\nnull.x": "TypeError: cannot read property 'x' of null (line 5)",
		// A syntax error at the end of the input is reported on the
		// wrapper's closing line; it is clamped to the last line of the code.
		"return {":              "SyntaxError: expecting ';' (line 1)",
		"const a = 1\nreturn [": "(line 2)",
		// Frames of code the script compiled itself with eval are skipped.
		// Code it compiled with new Function has the same two leading
		// lines as the agent's code, so its frames cannot be told apart.
		"const a = 1\neval(\"throw new Error('e')\")": "Error: e (line 2)",
	}
	for code, want := range cases {
		_, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, code)
		require.Error(t, err, code)
		assert.Contains(t, err.Error(), want, code)
	}
}

// TestScriptLogKeepsNUL: lines cross into Go with their length.
func TestScriptLogKeepsNUL(t *testing.T) {
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `console.log("a\0b")
return "c\0d"`)
	require.NoError(t, err)
	assert.Equal(t, "\"c\\u0000d\"\nlog: a\x00b", out)
}

// TestScriptRefusedCallsCount: every call attempt counts toward MaxCalls,
// also unknown, invalid and refused ones.
func TestScriptRefusedCallsCount(t *testing.T) {
	caller := newFakeScriptCaller()
	code := `const attempts = [
  () => api.packages.register_package({}),
  () => api.units.list_units(5),
  () => api.units.list_units({q: "x".repeat(1 << 20)}),
]
for (const attempt of attempts) {
  try { attempt() } catch (e) {}
}
try { api.units.list_units() } catch (e) { return e.message }`

	out, err := runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{MaxCalls: 3}, code)
	require.NoError(t, err)
	assert.Equal(t, `"script exceeded 3 function calls"`, out)
	assert.Empty(t, caller.calls)
}

// TestScriptConcurrentRuns: one engine runs scripts at once.
func TestScriptConcurrentRuns(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `console.log("x")
return api.units.list_units().total`)
			assert.NoError(t, err)
			assert.Equal(t, "2\nlog: x", out)
		}()
	}
	wg.Wait()
}

// TestScriptPromiseResult: a returned promise is settled after the pending
// jobs run.
func TestScriptPromiseResult(t *testing.T) {
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `return (async () => api.units.list_units().total)()`)
	require.NoError(t, err)
	assert.Equal(t, "2", out)

	_, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `return Promise.reject(new Error("no"))`)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "mcp: script error: Error: no (line 1)")

	_, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `return new Promise(() => {})`)
	require.Error(t, err)
	assert.Equal(t, "mcp: script error: TypeError: the script returned a promise that never settles", ez.ErrorMessage(err))
}

// TestScriptTrivialRunsAllocateLittle: an instance takes only the memory
// the script uses. Reserving the 64 MiB maximum up front cost that much
// zeroed heap for every run.
func TestScriptTrivialRunsAllocateLittle(t *testing.T) {
	mapping, err := newGuestMemory()
	if err != nil {
		t.Skip("no memory mapping on this platform")
	}
	mapping.release()
	cat := scriptTestCatalog()
	caller := newFakeScriptCaller()
	const runs = 20
	allocated := func() uint64 {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.TotalAlloc
	}
	before := allocated()
	for i := 0; i < runs; i++ {
		_, err := runScript(context.Background(), cat, caller, ScriptConfig{}, `return 1`)
		require.NoError(t, err)
	}
	perRun := (allocated() - before) / runs
	assert.Less(t, perRun, uint64(2<<20), "allocated per trivial run")
}

// TestScriptRefusedPayloadsAreNotCopied: a log line past the cap and the
// arguments of a call refused by the read-only rule or the budget stay in
// the guest. The script replaces the slice the shim uses, so only the Go
// side bounds the copy.
func TestScriptRefusedPayloadsAreNotCopied(t *testing.T) {
	code := `String.prototype.slice = function () { return String(this) }
const big = "q".repeat(256 << 10)
for (let i = 0; i < 200; i++) { console.log(big) }
const args = {q: big}
for (let i = 0; i < 200; i++) { try { api.packages.register_package(args) } catch (e) {} }
for (let i = 0; i < 100; i++) { try { api.units.list_units({}) } catch (e) {} }
return 1`
	allocated := func() uint64 {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.TotalAlloc
	}
	before := allocated()
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{MaxCalls: 10, Timeout: 30 * time.Second}, code)
	require.NoError(t, err)
	// What fits of the first line is kept, then logging stops.
	assert.True(t, strings.HasPrefix(out, "1\nlog: q"))
	assert.True(t, strings.HasSuffix(out, "\nlog: [truncated]"))
	assert.Len(t, strings.Split(out, "\n"), 3)
	// Copying every refused payload would take 100 MiB; the guest's own
	// memory grows by a few MiB.
	assert.Less(t, allocated()-before, uint64(8<<20), "allocated by a run whose payloads are all refused")
}

// TestScriptCallErrorClassification: traps the script can cause are its
// error; an abort inside the engine is a failure of the runtime.
func TestScriptCallErrorClassification(t *testing.T) {
	cases := map[string]string{
		"wasm error: stack overflow":                ez.EINVALID,
		"wasm error: out of bounds memory access":   ez.EINVALID,
		"wasm error: unreachable\nwasm stack trace": ez.EINTERNAL,
		"something else":                            ez.EINTERNAL,
	}
	for message, want := range cases {
		assert.Equal(t, want, ez.ErrorCode(scriptCallError(errors.New(message))), message)
	}
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(scriptCallError(scriptMemoryError())))
	// A wrapped trap is still a trap.
	wrapped := ez.New(ez.EINTERNAL, "mcp: the call failed", fmt.Errorf("malloc: %w", errors.New("wasm error: stack overflow")))
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(scriptCallError(wrapped)))
}

// TestScriptProtoModule: a module or function named __proto__ is a plain
// property of api.
func TestScriptProtoModule(t *testing.T) {
	get := &Function{Name: "__proto__.get", Module: "__proto__", ReadOnly: true}
	module := &Module{Name: "__proto__", Functions: []*Function{get}}
	cat := &Catalog{
		Modules:   []*Module{module},
		functions: map[string]*Function{get.Name: get},
		modules:   map[string]*Module{module.Name: module},
	}
	caller := newFakeScriptCaller()
	caller.responses[get.Name] = `7`

	out, err := runScript(context.Background(), cat, caller, ScriptConfig{}, `return [Object.keys(api), api.__proto__.get()]`)
	require.NoError(t, err)
	assert.Equal(t, `[["__proto__"],7]`, out)
}

// TestScriptMemoryLimitHolds: allocations that land exactly on the room
// left under the limit do not open it. malloc rounds requests up, so the
// accounting could pass the limit and wrap, after which every allocation
// passed until linear memory ran out.
func TestScriptMemoryLimitHolds(t *testing.T) {
	code := `const held = []
let total = 0
for (let size = 1 << 20; size >= 1024; size >>= 1) {
	try { for (;;) { held.push(new ArrayBuffer(size)); total += size } } catch (e) {}
}
for (let size = 2000; size >= 16; size--) {
	try { held.push(new ArrayBuffer(size)); total += size } catch (e) {}
}
try { for (;;) { held.push(new ArrayBuffer(1 << 20)); total += 1 << 20 } } catch (e) {}
return total`
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{Timeout: 30 * time.Second}, code)
	require.NoError(t, err)
	total, err := strconv.Atoi(out)
	require.NoError(t, err)
	assert.Less(t, total, 49<<20, "bytes held under a 48 MiB limit")
	assert.Greater(t, total, 40<<20, "bytes held under a 48 MiB limit")
}

// TestScriptHeapFallback: where mapping the guest's memory is unsupported,
// runs go on with wazero's own growth on the Go heap and nothing is
// reported; where it is supported but fails, the run is refused.
func TestScriptHeapFallback(t *testing.T) {
	engine, err := testScriptEngine()
	require.NoError(t, err)
	mapGuestMemoryFunc = func(size int) ([]byte, error) {
		return nil, ez.New(ez.EUNAVAILABLE, "mcp: no mapping in this test", nil)
	}
	defer func() {
		mapGuestMemoryFunc = mapGuestMemory
	}()
	var reported []error
	onError := func(ctx context.Context, err error) {
		reported = append(reported, err)
	}

	out, err := engine.run(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `return api.units.list_units().total`, onError)
	require.NoError(t, err)
	assert.Equal(t, "2", out)
	_, err = engine.run(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `const a = []; for (;;) { a.push(new ArrayBuffer(1 << 20)) }`, onError)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of memory")
	assert.Empty(t, reported)

	mapGuestMemoryFunc = func(size int) ([]byte, error) {
		return nil, ez.New(ez.ERESOURCEEXHAUSTED, "mcp: cannot map memory for the script; try again shortly", nil)
	}
	_, err = engine.run(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `return 1`, onError)
	require.Error(t, err)
	assert.Equal(t, ez.ERESOURCEEXHAUSTED, ez.ErrorCode(err))
	require.Len(t, reported, 1)
	assert.Equal(t, ez.ERESOURCEEXHAUSTED, ez.ErrorCode(reported[0]))
}

// pendingWriteCaller answers a write only when the context ends.
type pendingWriteCaller struct{}

func (pendingWriteCaller) Call(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	if name == "packages.register_package" {
		<-ctx.Done()
		return nil, ez.New(ez.ECANCELED, "mcp: canceled", ctx.Err())
	}
	return json.RawMessage(`{}`), nil
}

// TestScriptWritesListedOnTimeout: a write still running at the deadline
// is listed, since it may have changed data.
func TestScriptWritesListedOnTimeout(t *testing.T) {
	_, err := runScript(context.Background(), scriptTestCatalog(), pendingWriteCaller{}, ScriptConfig{AllowWrites: true, Timeout: 50 * time.Millisecond}, `api.packages.register_package({name: "x"})`)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Equal(t, "mcp: script failed after calls that write, which may have changed data (packages.register_package): script timed out after 50ms", ez.ErrorMessage(err))
}

// TestScriptPrepareStackTraceIsFixed: Error.prepareStackTrace would hand
// the functions of the stack, the shim's included, to the script. Sloppy
// code that assigns it is ignored and strict code gets a TypeError.
func TestScriptPrepareStackTraceIsFixed(t *testing.T) {
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `Error.prepareStackTrace = () => "x"
return [Error.prepareStackTrace === undefined, new Error("a").stack.includes("<input>")]`)
	require.NoError(t, err)
	assert.Equal(t, `[true,true]`, out)

	_, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `Error.prepareStackTrace = (e, frames) => frames
throw new Error("boom")`)
	require.Error(t, err)
	assert.Equal(t, "mcp: script error: Error: boom (line 2)", ez.ErrorMessage(err))

	_, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `"use strict"
Error.prepareStackTrace = () => "x"`)
	require.Error(t, err)
	assert.Contains(t, ez.ErrorMessage(err), "mcp: script error: TypeError")
	assert.Contains(t, ez.ErrorMessage(err), "(line 2)")
}

// TestScriptCutsWholeCharacters: a log line, a thrown message and the
// message of a failed call cut at their byte caps stay valid UTF-8.
func TestScriptCutsWholeCharacters(t *testing.T) {
	out, err := runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `console.log("é".repeat(40000))`)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(out, "\nlog: [truncated]"))
	assert.True(t, utf8.ValidString(out))

	_, err = runScript(context.Background(), scriptTestCatalog(), newFakeScriptCaller(), ScriptConfig{}, `throw new Error("é".repeat(10000))`)
	require.Error(t, err)
	message := ez.ErrorMessage(err)
	assert.True(t, utf8.ValidString(message))
	assert.True(t, strings.HasSuffix(message, "é [truncated] (line 1)"), message[len(message)-40:])

	caller := newFakeScriptCaller()
	caller.errors["units.get_unit"] = ez.New(ez.ENOTFOUND, strings.Repeat("é", 10000), nil)
	out, err = runScript(context.Background(), scriptTestCatalog(), caller, ScriptConfig{}, `try { api.units.get_unit({}) } catch (e) { return e.message }`)
	require.NoError(t, err)
	assert.True(t, utf8.ValidString(out))
	assert.True(t, strings.HasSuffix(out, `é [truncated]"`), out[len(out)-40:])

	// Bytes that were invalid before the cut are not all erased.
	assert.Equal(t, "ab\xff", scriptCut("ab\xff\xff\xff\xff\xff", 6))
	assert.Equal(t, "a", scriptCut("aé", 2))
	assert.Equal(t, "aé", scriptCut("aé", 3))
}
