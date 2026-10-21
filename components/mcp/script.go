package mcp

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
	"github.com/vanclief/ez"
)

const (
	defaultScriptTimeout       = 5 * time.Second
	defaultScriptMaxCalls      = 200
	defaultScriptMaxConcurrent = 4
	// maxScriptBytes caps the source of one script.
	maxScriptBytes = 64 * 1024
	// maxScriptLogLines and maxScriptLogBytes cap the console.log output
	// kept; later lines are dropped.
	maxScriptLogLines = 200
	maxScriptLogBytes = 64 * 1024
	// maxScriptNameBytes caps the name of a function a script calls; no
	// catalog name comes near it, so a longer one is unknown without being
	// read.
	maxScriptNameBytes = 1024
	// maxScriptArgumentBytes caps the JSON arguments of one function call.
	maxScriptArgumentBytes = 1 << 20
	// maxScriptResponseBytes caps a function response handed to a script;
	// a larger one is an error and is not copied into the guest.
	maxScriptResponseBytes = 4 << 20
	// maxScriptResultBytes caps the JSON a script returns, in bytes. Go
	// checks the length before it reads the text out of the guest.
	maxScriptResultBytes = 1 << 20
	// scriptMemoryPages is the linear memory maximum, 64 MiB, which the
	// embedded binary declares too.
	scriptMemoryPages = 1024
	scriptMemoryBytes = scriptMemoryPages << 16
	// maxScriptFailureBytes caps the message of a failed call handed to
	// the script; a handler may echo what the script sent.
	maxScriptFailureBytes = 16 << 10
	// maxScriptErrorBytes caps the message of a thrown value in the error
	// of a run, so the error survives the output cap of the tool.
	maxScriptErrorBytes = 8 << 10
	// maxScriptWriteNames caps the functions a failed run lists as called.
	maxScriptWriteNames = 20
	// scriptMemoryLimit is the QuickJS heap limit. It sits below the linear
	// memory maximum so an allocation bomb ends in a catchable
	// "InternalError: out of memory" while wasi-libc's allocator still has
	// room for the 1 MiB stack, the static data, its own overhead and the
	// replies of function calls.
	scriptMemoryLimit = 48 << 20
	// scriptStackLimit is the QuickJS stack limit. It sits below the 1 MiB
	// stack quickjs/build.sh links, so a deep recursion throws a catchable
	// RangeError, about 3000 frames deep, and native code past the last
	// check still has 64 KiB.
	scriptStackLimit = 960 << 10
	// scriptSlotWait is how long a script waits for a free slot when
	// MaxConcurrent scripts are already running.
	scriptSlotWait = time.Second
)

// scriptUsage is the contract shown to the agent in the run_script tool
// description. The verbs are the timeout, the call limit, the write rule
// and the result limit; scriptDescription fills them.
const scriptUsage = `Run a short JavaScript program that chains several function calls in one round trip. The program is the body of a function: use return to send back the result, encoded as JSON. Call api.<module>.<function>({...}) with the same flat arguments as call_function; the call is synchronous and returns the parsed response, or throws an Error whose message carries the REST error code (also in e.code), so try/catch works. console.log(...) lines are returned after the result. Limits: %s timeout, %d function calls, %s, %d KiB of result JSON, no network, filesystem, timers or modules. Use search_functions and describe_function to learn which functions exist and their arguments.`

// scriptDescription is the run_script tool description with the configured
// limits.
func scriptDescription(cfg ScriptConfig) string {
	timeout, maxCalls := scriptLimits(cfg)
	writes := "read-only functions only"
	if cfg.AllowWrites {
		writes = "functions that write are allowed"
	}
	return fmt.Sprintf(scriptUsage, timeout, maxCalls, writes, maxScriptResultBytes>>10)
}

// scriptLimits applies the ScriptConfig defaults.
func scriptLimits(cfg ScriptConfig) (time.Duration, int) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultScriptTimeout
	}
	maxCalls := cfg.MaxCalls
	if maxCalls <= 0 {
		maxCalls = defaultScriptMaxCalls
	}
	return timeout, maxCalls
}

// quickjsWasm is QuickJS compiled to WebAssembly. Scripts run in it on
// wazero, so a script cannot reach Go memory or the Go stack, and the
// context deadline is checked inside the compiled code, native builtins
// included.
//
// Provenance: quickjs/build.sh links quickjs/sandbox.c with the engine of
// quickjs-ng v0.17.0 and records the toolchain, flags and sha256 in
// quickjs/README.md. It has no quickjs-libc, so there is no std, os or
// bjson module, no print, scriptArgs or gc global, and no module loader.
// The 1 MiB C stack sits at the bottom of linear memory, so native code
// that overflows it traps below address 0 instead of overwriting static
// data, and the memory declares a maximum of 1024 pages (64 MiB).
//
// TestScriptWasmMemoryIsCapped checks the checksum, the cap and the stack
// placement.
//
//go:embed quickjs.wasm
var quickjsWasm []byte

// The kinds of text the guest's run export returns. See quickjs/sandbox.c.
const (
	scriptResult = 0
	scriptThrown = 1
	scriptFailed = 2
)

// scriptStackError is the error of a run that trapped: a native recursion
// past the stack, or a memory fault.
func scriptStackError() error {
	return ez.New(ez.EINVALID, "mcp: script error: the script exhausted the runtime stack or memory", nil)
}

// scriptMemoryError is the error of a run whose output did not fit in the
// guest's memory.
func scriptMemoryError() error {
	return ez.New(ez.EINVALID, "mcp: script error: the script ran out of memory", nil)
}

// scriptModuleConfig instantiates the guest with a clock only: no
// filesystem, environment, arguments or randomness (the module imports no
// random_get), and stdout and stderr discarded. The empty name keeps
// instances anonymous, so they can run at once.
var scriptModuleConfig = wazero.NewModuleConfig().
	WithName("").
	WithStartFunctions("_initialize").
	WithSysWalltime().
	WithSysNanotime()

// sharedScriptEngine is the engine of the process, built on first use and
// never closed. An engine has no per-Server state: the limits are constants
// and the host functions take the run's state from the context. One engine
// per Server would keep a compiled copy of the module per Server and leave
// wazero entries for the host modules of every Server ever created.
var sharedScriptEngine = sync.OnceValues(newScriptEngine)

// scriptEngine runs scripts. It holds one wazero runtime with the WASI
// functions, the env host module and the compiled QuickJS module, all built
// once; every run instantiates the compiled module anew and closes the
// instance when it ends, so nothing accumulates from one run to the next.
type scriptEngine struct {
	runtime     wazero.Runtime
	module      wazero.CompiledModule
	memoryLimit uint32
	stackLimit  uint32
}

// newScriptEngine builds an engine: it compiles the QuickJS module, which
// takes about 300 ms. Servers use sharedScriptEngine; tests build their own
// when they need to break one. Each run backs its linear memory with a
// guestMemory.
func newScriptEngine() (*scriptEngine, error) {
	ctx := context.Background()
	config := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(scriptMemoryPages)
	e := &scriptEngine{
		runtime:     wazero.NewRuntimeWithConfig(ctx, config),
		memoryLimit: scriptMemoryLimit,
		stackLimit:  scriptStackLimit,
	}
	err := e.load(ctx)
	if err != nil {
		_ = e.runtime.Close(ctx)
		return nil, ez.New(ez.EINTERNAL, "mcp: cannot start the script runtime", err)
	}
	return e, nil
}

// load instantiates the host modules and compiles the guest.
func (e *scriptEngine) load(ctx context.Context) error {
	_, err := wasi_snapshot_preview1.Instantiate(ctx, e.runtime)
	if err != nil {
		return err
	}
	i32 := api.ValueTypeI32
	_, err = e.runtime.NewHostModuleBuilder("env").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(scriptHostCall), []api.ValueType{i32, i32, i32, i32, i32}, []api.ValueType{i32}).
		Export("host_call").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(scriptHostLog), []api.ValueType{i32, i32}, nil).
		Export("host_log").
		Instantiate(ctx)
	if err != nil {
		return err
	}
	e.module, err = e.runtime.CompileModule(ctx, quickjsWasm)
	return err
}

// guestMemory backs the linear memory of one instance with an anonymous
// mapping of the whole maximum: the kernel commits pages as the guest
// touches them, growth is a reslice, nothing is copied and nothing lands on
// the Go heap. Without it wazero grows memory by appending to a Go slice,
// which copied the buffer over a dozen times on the way to the maximum and
// allocated 275 MiB of heap per memory-filling run; reserving the maximum
// up front instead cost 64 MiB of zeroed heap for every run.
type guestMemory struct {
	buf []byte
}

// mapGuestMemoryFunc and unmapGuestMemoryFunc are mapGuestMemory and
// unmapGuestMemory, replaceable by tests that exercise the Go-heap fallback
// or count the mappings.
var mapGuestMemoryFunc = mapGuestMemory

var unmapGuestMemoryFunc = unmapGuestMemory

// newGuestMemory maps a buffer of the maximum size. It returns nil and an
// EUNAVAILABLE error where mapping is unsupported, in which case wazero
// grows the memory on the Go heap, and ERESOURCEEXHAUSTED when mapping
// failed.
func newGuestMemory() (*guestMemory, error) {
	buf, err := mapGuestMemoryFunc(scriptMemoryBytes)
	if err != nil {
		return nil, ez.Wrap(err)
	}
	return &guestMemory{buf: buf}, nil
}

func (m *guestMemory) Allocate(initial, maximum uint64) experimental.LinearMemory {
	return m
}

func (m *guestMemory) Reallocate(size uint64) []byte {
	if size > uint64(len(m.buf)) {
		return nil
	}
	return m.buf[:size]
}

// Free does nothing: wazero calls it when it closes the instance, which can
// happen from a nested call while the guest still runs. release unmaps the
// buffer once the run is over.
func (m *guestMemory) Free() {}

func (m *guestMemory) release() {
	unmapGuestMemoryFunc(m.buf)
}

// scriptShim runs before the agent's code and evaluates to the object the
// guest's run export drives: run compiles the code with new Function and
// calls it. The code arrives as a string argument and is never spliced into
// source the shim evaluates; new Function compiles it at global scope, so
// code that closes its function body early only reaches global scope, where
// the shim keeps nothing it depends on. encode and describe
// turn the result and a thrown value into text with the builtins captured
// before the agent's code could replace them. The shim takes the two host
// functions off the global object and builds api.<module>.<function> and
// console.log. Go splices in the function names and the log line limit. Go
// makes every check; the shim only shapes the calls.
const scriptShim = `(function (names, writes, maxLogBytes, maxErrorChars) {
	"use strict";
	const call = globalThis.__call;
	const log = globalThis.__log;
	delete globalThis.__call;
	delete globalThis.__log;
	const ScriptFunction = Function;
	const ScriptError = Error;
	const ScriptString = String;
	const stringify = JSON.stringify;
	const parse = JSON.parse;
	const apply = Reflect.apply;
	const toString = Object.prototype.toString;
	const hasOwn = Object.prototype.hasOwnProperty;
	const slice = String.prototype.slice;
	const freeze = Object.freeze;
	// quickjs-ng calls Error.prepareStackTrace with CallSite objects whose
	// getFunction() hands out the real functions of the stack, the shim's
	// included. A read-only undefined makes an assignment fail: silently in
	// sloppy code, with a TypeError in strict code.
	Object.defineProperty(ScriptError, "prepareStackTrace", { value: undefined, writable: false, configurable: false });
	const api = Object.create(null);
	// Built now: when a reply cannot be read for lack of memory, throwing
	// this needs none.
	const unread = freeze(new ScriptError("out of memory: the response could not be read"));
	for (const name of names) {
		const dot = name.indexOf(".");
		const module = name.slice(0, dot);
		if (!hasOwn.call(api, module)) {
			api[module] = Object.create(null);
		}
		// A function that writes gets its own, since its changes stay.
		const lost = hasOwn.call(writes, name) ? freeze(new ScriptError(name + " may have been called, but its response could not be read; any changes it made stay")) : null;
		api[module][name.slice(dot + 1)] = function (args) {
			const text = args === undefined || args === null ? "" : ScriptString(stringify(args));
			// call throws only before the host ran; an unread reply is undefined.
			const raw = call(name, text);
			if (raw === undefined) {
				throw lost === null ? unread : lost;
			}
			let reply;
			let e;
			try {
				reply = parse(raw);
				if (reply.ok) {
					return reply.value;
				}
				e = new ScriptError(reply.message);
				if (reply.code) {
					e.code = reply.code;
				}
			} catch (thrown) {
				throw lost === null ? thrown : lost;
			}
			throw e;
		};
	}
	const format = function (value) {
		if (typeof value === "string") {
			return value;
		}
		if (value === undefined) {
			return "undefined";
		}
		if (value instanceof ScriptError) {
			return ScriptString(value);
		}
		try {
			const text = stringify(value);
			if (typeof text === "string") {
				return text;
			}
		} catch (e) {
		}
		return apply(toString, value, []);
	};
	globalThis.api = api;
	globalThis.console = {
		log: function (...values) {
			let line = values.map(format).join(" ");
			if (line.length > maxLogBytes) {
				line = line.slice(0, maxLogBytes);
			}
			log(line);
		},
	};
	return {
		run: function (code) {
			return new ScriptFunction(code)();
		},
		encode: function (value) {
			const text = stringify(value);
			return typeof text === "string" ? text : "null";
		},
		describe: function (e) {
			if (e instanceof ScriptError) {
				const stack = e.stack;
				let message = ScriptString(e);
				if (message.length > maxErrorChars) {
					message = apply(slice, message, [0, maxErrorChars]) + " [truncated]";
				}
				return typeof stack === "string" ? message + "\n" + apply(slice, stack, [0, maxErrorChars]) : message;
			}
			if (e !== null && (typeof e === "object" || typeof e === "function")) {
				return apply(toString, e, []);
			}
			return ScriptString(e);
		},
	};
})`

// scriptCodeLines is the number of lines new Function puts before the
// agent's code: "(function anonymous(" and ") {".
const scriptCodeLines = 2

// scriptLine finds a frame of the agent's code in a line of a stack; code
// compiled by new Function and eval is named <input>.
var scriptLine = regexp.MustCompile(`<input>:(\d+)`)

// scriptJSONFrame is the stack line after the frame JSON.parse adds for the
// position of a syntax error in its input, "at <input>:1:2", which is not a
// line of the agent's code.
const scriptJSONFrame = "at parse (native)"

// scriptRunKey is the context key of the scriptRun the host functions
// serve.
type scriptRunKey struct{}

// scriptRun is the state of one execution: what the host functions need
// and what they collected.
type scriptRun struct {
	cat      *Catalog
	call     Caller
	cfg      ScriptConfig
	maxCalls int
	report   func(error)
	calls    int
	logs     []string
	logBytes int
	logCut   bool
	// fault is the error of a call into the guest that trapped inside a
	// host function; the run fails with it even if the script went on.
	fault error
	// malloc is the guest's allocator, looked up once per instance.
	malloc api.Function
	// wrote names the functions that write and were called, whatever came
	// of the call, so a run that fails can still say what may have
	// changed.
	wrote []string
}

// run runs code as the body of a function in a fresh instance of the
// QuickJS module whose global api object exposes every catalog function. It
// returns the JSON of the returned value followed by the console.log lines,
// so the output cap of the tool cuts logs, not the result. On error the log
// lines collected so far are returned with the error. The
// timeout bounds the whole run, function calls included. onError, when not
// nil, receives the panics of function calls, which the script sees only
// as a generic error.
func (e *scriptEngine) run(ctx context.Context, cat *Catalog, call Caller, cfg ScriptConfig, code string, onError func(context.Context, error)) (string, error) {
	timeout, maxCalls := scriptLimits(cfg)
	if len(code) > maxScriptBytes {
		return "", ez.New(ez.EINVALID, fmt.Sprintf("mcp: script is %d bytes; the limit is %d", len(code), maxScriptBytes), nil)
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	run := &scriptRun{
		cat:      cat,
		call:     call,
		cfg:      cfg,
		maxCalls: maxCalls,
		report: func(err error) {
			if onError != nil {
				onError(parent, err)
			}
		},
	}
	result, err := e.execute(context.WithValue(ctx, scriptRunKey{}, run), run, code)
	if err != nil {
		if ctx.Err() != nil {
			err = interruptError(parent, timeout)
		}
		return strings.Join(run.logs, "\n"), run.withWrites(err)
	}
	return strings.Join(append([]string{result}, run.logs...), "\n"), nil
}

// withWrites puts the functions that write and were called before a run
// failed in front of its error, so the agent does not repeat them: the
// script may never have seen their responses. The list comes before the
// script's own message, so the message cannot forge one.
func (r *scriptRun) withWrites(err error) error {
	if len(r.wrote) == 0 {
		return err
	}
	names := r.wrote
	more := ""
	if len(names) > maxScriptWriteNames {
		more = fmt.Sprintf(" and %d more", len(names)-maxScriptWriteNames)
		names = names[:maxScriptWriteNames]
	}
	message := fmt.Sprintf("mcp: script failed after calls that write, which may have changed data (%s%s): %s", strings.Join(names, ", "), more, errorMessage(err))
	return ez.New(ez.ErrorCode(err), message, err)
}

// execute instantiates the guest, hands it the shim and the code and reads
// what its run export returns. A wasm trap, the closing of the instance at
// the deadline included, comes back as an error from the call.
func (e *scriptEngine) execute(ctx context.Context, run *scriptRun, code string) (string, error) {
	mapping, err := newGuestMemory()
	if err != nil && ez.ErrorCode(err) != ez.EUNAVAILABLE {
		// Mapping is supported but failed, for lack of memory: growing on
		// the Go heap instead could take the process down. The operator
		// hears of it, since a limit on the process makes this permanent.
		if run.report != nil {
			run.report(err)
		}
		return "", ez.Wrap(err)
	}
	if mapping != nil {
		// Released after the instance is closed: wazero may close the
		// instance from a nested call while the outer guest frame still
		// runs on this memory.
		defer mapping.release()
		ctx = experimental.WithMemoryAllocator(ctx, mapping)
	}
	mod, err := e.runtime.InstantiateModule(ctx, e.module, scriptModuleConfig)
	if err != nil {
		return "", ez.New(ez.EINTERNAL, "mcp: cannot start the script runtime", err)
	}
	defer func() {
		_ = mod.Close(ctx)
	}()
	run.malloc = mod.ExportedFunction("malloc")
	memory := mod.Memory()

	shim, err := scriptShimText(run.cat, run.cfg.AllowWrites)
	if err != nil {
		return "", ez.Wrap(err)
	}
	shimPtr, err := run.put(ctx, memory, shim)
	if err != nil {
		return "", scriptCallError(err)
	}
	codePtr, err := run.put(ctx, memory, code)
	if err != nil {
		return "", scriptCallError(err)
	}
	outPtr, err := run.alloc(ctx, 8)
	if err != nil {
		return "", scriptCallError(err)
	}
	if shimPtr == 0 || codePtr == 0 || outPtr == 0 {
		return "", scriptMemoryError()
	}
	results, err := mod.ExportedFunction("run").Call(ctx,
		uint64(e.memoryLimit), uint64(e.stackLimit),
		uint64(shimPtr), uint64(len(shim)),
		uint64(codePtr), uint64(len(code)),
		uint64(outPtr), uint64(outPtr+4))
	if err != nil {
		return "", scriptCallError(err)
	}
	if run.fault != nil {
		return "", scriptCallError(run.fault)
	}

	textPtr, _ := memory.ReadUint32Le(outPtr)
	textLen, _ := memory.ReadUint32Le(outPtr + 4)
	switch results[0] {
	case scriptResult:
		if textLen > maxScriptResultBytes {
			return "", ez.New(ez.EINVALID, fmt.Sprintf("mcp: script error: the result is %d bytes of JSON; the limit is %d, return less", textLen, maxScriptResultBytes), nil)
		}
		text, ok := guestText(memory, textPtr, textLen)
		if !ok {
			return "", scriptMemoryError()
		}
		return text, nil
	case scriptThrown:
		text, ok := guestText(memory, textPtr, min(textLen, maxScriptResultBytes))
		if !ok {
			return "", scriptMemoryError()
		}
		return "", scriptException(text, code)
	default:
		text, _ := guestText(memory, textPtr, min(textLen, maxScriptResultBytes))
		return "", ez.New(ez.EINTERNAL, "mcp: the script runtime failed: "+firstLine(text), nil)
	}
}

// scriptShimText is the shim applied to the catalog's function names, the
// names of those that may write as the keys of an object, and the caps.
func scriptShimText(cat *Catalog, allowWrites bool) (string, error) {
	names := []string{}
	writes := map[string]bool{}
	for _, f := range cat.Functions() {
		names = append(names, f.Name)
		if allowWrites && !f.ReadOnly {
			writes[f.Name] = true
		}
	}
	namesJSON, err := json.Marshal(names)
	if err != nil {
		return "", ez.New(ez.EINTERNAL, "mcp: cannot encode the function names", err)
	}
	writesJSON, err := json.Marshal(writes)
	if err != nil {
		return "", ez.New(ez.EINTERNAL, "mcp: cannot encode the names of the functions that write", err)
	}
	return scriptShim + "(" + string(namesJSON) + ", " + string(writesJSON) + ", " + strconv.Itoa(maxScriptLogBytes) + ", " + strconv.Itoa(maxScriptErrorBytes) + ")", nil
}

// scriptCallError maps the error of a call into the guest made while the
// run's context is live. wazero reports traps as errors starting with
// "wasm error: " around an internal type, so the prefix is the only public
// way to tell them apart. A stack overflow or a memory fault is the
// script's doing; unreachable is an abort() inside the engine, which is a
// failure of the runtime.
func scriptCallError(err error) error {
	var exit *sys.ExitError
	if errors.As(err, &exit) {
		return scriptStackError()
	}
	// Wrappers above the trap prefix their operation to the text, and
	// wazero's own error unwraps to the bare reason, so every level is
	// looked at.
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		message := cause.Error()
		if strings.HasPrefix(message, "wasm error: ") && !strings.HasPrefix(message, "wasm error: unreachable") {
			return scriptStackError()
		}
	}
	var ezErr *ez.Error
	if errors.As(err, &ezErr) {
		return ez.Wrap(err)
	}
	return ez.New(ez.EINTERNAL, "mcp: the script runtime failed: "+firstLine(err.Error()), err)
}

// alloc allocates size bytes with the guest's malloc. It returns 0 when the
// guest has no memory left. A failed call is a trap; its error is wazero's,
// returned as is so scriptCallError can classify it.
func (r *scriptRun) alloc(ctx context.Context, size int) (uint32, error) {
	results, err := r.malloc.Call(ctx, uint64(size))
	if err != nil {
		return 0, err
	}
	return uint32(results[0]), nil
}

// put copies text into guest memory followed by a NUL and returns its
// address, 0 when the guest has no memory for it.
func (r *scriptRun) put(ctx context.Context, memory api.Memory, text string) (uint32, error) {
	ptr, err := r.alloc(ctx, len(text)+1)
	if err != nil || ptr == 0 {
		return 0, err
	}
	if !memory.WriteString(ptr, text) || !memory.WriteByte(ptr+uint32(len(text)), 0) {
		return 0, ez.New(ez.EINTERNAL, "mcp: the script runtime returned memory out of range", nil)
	}
	return ptr, nil
}

// guestText copies length bytes at ptr out of guest memory. A zero ptr is
// a text the guest could not allocate.
func guestText(memory api.Memory, ptr, length uint32) (string, bool) {
	if ptr == 0 {
		return "", false
	}
	data, ok := memory.Read(ptr, length)
	if !ok {
		return "", false
	}
	return string(data), true
}

// scriptHostCall is env.host_call(name_ptr, name_len, args_ptr, args_len,
// reply_len_ptr) -> reply_ptr: it runs one catalog function and returns the
// JSON reply the shim unpacks, in memory taken with the guest's malloc, or
// 0 when the guest has no memory for it. The arguments are copied out of
// the guest only once the call is admitted, so a script cannot make Go
// copy megabytes for calls that are refused anyway.
func scriptHostCall(ctx context.Context, mod api.Module, stack []uint64) {
	run, ok := ctx.Value(scriptRunKey{}).(*scriptRun)
	if !ok {
		stack[0] = 0
		return
	}
	memory := mod.Memory()
	replyLenPtr := uint32(stack[4])

	name, reply := run.admit(memory, uint32(stack[0]), uint32(stack[1]), uint32(stack[3]))
	ran := false
	if reply == "" {
		text, ok := guestText(memory, uint32(stack[2]), uint32(stack[3]))
		if ok {
			reply, ran = run.safeReply(ctx, name, text)
		} else {
			reply = scriptFailure("", name+": the call cannot be read")
		}
	}
	ptr, err := run.put(ctx, memory, reply)
	if err == nil && ptr == 0 {
		reply = run.lostReply(name, ran)
		ptr, err = run.put(ctx, memory, reply)
	}
	if err != nil {
		run.fault = err
	}
	if err != nil || ptr == 0 {
		stack[0] = 0
		return
	}
	memory.WriteUint32Le(replyLenPtr, uint32(len(reply)))
	stack[0] = uint64(ptr)
}

// admit counts one call attempt and makes the checks that need no
// arguments: the call budget, the function, the size of the arguments and
// the read-only rule. It returns the function name and the failure reply of
// a refused call, "" when the call may proceed. Every attempt counts toward
// MaxCalls, refused ones included.
func (r *scriptRun) admit(memory api.Memory, namePtr, nameLen, argsLen uint32) (string, string) {
	if r.fault != nil {
		return "", scriptFailure("", "the script runtime failed")
	}
	r.calls++
	if r.calls > r.maxCalls {
		return "", scriptFailure("", fmt.Sprintf("script exceeded %d function calls", r.maxCalls))
	}
	name := ""
	if nameLen <= maxScriptNameBytes {
		name, _ = guestText(memory, namePtr, nameLen)
	}
	f, ok := r.cat.Function(name)
	if !ok {
		return name, scriptFailure("", "unknown function "+name)
	}
	if argsLen > maxScriptArgumentBytes {
		return name, scriptFailure("", fmt.Sprintf("%s: the arguments are %d bytes of JSON; the limit is %d", name, argsLen, maxScriptArgumentBytes))
	}
	if !r.cfg.AllowWrites && !f.ReadOnly {
		return name, scriptFailure("", name+" is not read-only; run_script only allows read-only functions on this server")
	}
	return name, ""
}

// lostReply is the reply of a call whose reply did not fit in the guest's
// memory.
func (r *scriptRun) lostReply(name string, ran bool) string {
	problem := "the script ran out of memory before the response could be delivered"
	if ran {
		return r.problemReply(name, problem)
	}
	return scriptFailure("", name+": "+problem)
}

// problemReply is the reply of a call that ran but whose response cannot
// be handed to the script. A function that writes may have changed data,
// and the script must hear that.
func (r *scriptRun) problemReply(name, problem string) string {
	f, ok := r.cat.Function(name)
	if ok && !f.ReadOnly {
		return scriptFailure("", name+" ran, and any changes it made stay, but "+problem)
	}
	return scriptFailure("", name+": "+problem)
}

// safeReply is reply with the panics of the caller turned into a generic
// failure, reported with their stack.
func (r *scriptRun) safeReply(ctx context.Context, name, text string) (reply string, ran bool) {
	defer func() {
		value := recover()
		if value == nil {
			return
		}
		r.report(ez.New(ez.EINTERNAL, fmt.Sprintf("mcp: run_script: %s panicked: %v\n%s", name, value, debug.Stack()), nil))
		reply = scriptFailure("", name+": the call failed")
		ran = true
	}()
	return r.reply(ctx, name, text)
}

// reply calls the admitted function with the JSON arguments text, "" for
// none, and returns the envelope {"ok":true,"value":...} or
// {"ok":false,...} and whether the function was called.
func (r *scriptRun) reply(ctx context.Context, name, text string) (string, bool) {
	args := map[string]any{}
	if text != "" {
		err := json.Unmarshal([]byte(text), &args)
		if err != nil || args == nil {
			return scriptFailure("", name+": arguments must be an object"), false
		}
	}
	f, _ := r.cat.Function(name)
	if f != nil && !f.ReadOnly && !slices.Contains(r.wrote, name) {
		r.wrote = append(r.wrote, name)
	}
	raw, err := r.call.Call(ctx, name, args)
	if err != nil {
		code := ez.ErrorCode(err)
		return scriptFailure(code, fmt.Sprintf("%s: %s: %s", name, code, errorMessage(err))), true
	}
	if len(raw) > maxScriptResponseBytes {
		return r.problemReply(name, fmt.Sprintf("the response is %d bytes; scripts accept at most %d, ask for less", len(raw), maxScriptResponseBytes)), true
	}
	if len(raw) == 0 {
		return `{"ok":true,"value":null}`, true
	}
	if !json.Valid(raw) {
		return r.problemReply(name, "the response is not JSON"), true
	}
	return `{"ok":true,"value":` + string(raw) + `}`, true
}

// scriptFailure is the reply the shim turns into a thrown Error with the
// message and, when set, e.code.
func scriptFailure(code, message string) string {
	if len(message) > maxScriptFailureBytes {
		message = scriptCut(message, maxScriptFailureBytes) + " [truncated]"
	}
	reply, err := json.Marshal(struct {
		OK      bool   `json:"ok"`
		Code    string `json:"code,omitempty"`
		Message string `json:"message"`
	}{Code: code, Message: message})
	if err != nil {
		return `{"ok":false,"message":"the error cannot be encoded"}`
	}
	return string(reply)
}

// scriptHostLog is env.host_log(ptr, len): it keeps a console.log line
// within the caps. Of a line that does not fit, only what fits is copied
// out of the guest; the shim's own slice runs on prototypes the script
// controls.
func scriptHostLog(ctx context.Context, mod api.Module, stack []uint64) {
	run, ok := ctx.Value(scriptRunKey{}).(*scriptRun)
	if !ok || run.logCut {
		return
	}
	size := int(uint32(stack[1]))
	room := maxScriptLogBytes - run.logBytes - len("log: ")
	if len(run.logs) >= maxScriptLogLines || room <= 0 {
		run.logs = append(run.logs, "log: [truncated]")
		run.logCut = true
		return
	}
	line, ok := guestText(mod.Memory(), uint32(stack[0]), uint32(min(size, room)))
	if !ok {
		return
	}
	if size > room {
		line = scriptCut(line, room)
	}
	// One line per log call: a line break in the text could forge the
	// lines of the tool result after it.
	line = scriptLineBreaks.Replace(line)
	run.logs = append(run.logs, "log: "+line)
	run.logBytes += len("log: ") + len(line)
	if size > room {
		run.logs = append(run.logs, "log: [truncated]")
		run.logCut = true
	}
}

// interruptError is the error of a script stopped because ctx, derived from
// parent with the timeout, is done.
func interruptError(parent context.Context, timeout time.Duration) error {
	err := parent.Err()
	if err != nil {
		return ez.New(ez.ECANCELED, fmt.Sprintf("mcp: script canceled: %v", err), nil)
	}
	return ez.New(ez.EINVALID, fmt.Sprintf("mcp: script timed out after %s", timeout), nil)
}

// scriptLineBreaks escapes every line break JavaScript or a terminal knows,
// and the vertical tab, form feed and escape characters, so a line cannot
// carry terminal control sequences either.
var scriptLineBreaks = strings.NewReplacer("\n", "\\n", "\r", "\\r", "\u0085", "\\u0085", "\u2028", "\\u2028", "\u2029", "\\u2029", "\v", "\\v", "\f", "\\f", "\x1b", "\\x1b")

// scriptCut cuts text to at most limit bytes, then drops the bytes of a
// character the cut split. It drops at most utf8.UTFMax-1 bytes, so text
// that was not valid UTF-8 to begin with is not erased.
func scriptCut(text string, limit int) string {
	text = text[:min(len(text), limit)]
	for range utf8.UTFMax - 1 {
		r, size := utf8.DecodeLastRuneInString(text)
		if r != utf8.RuneError || size != 1 {
			break
		}
		text = text[:len(text)-1]
	}
	return text
}

// scriptException is the error of a thrown value or syntax error: its text,
// then the first line of the agent's code in its stack, if any.
func scriptException(text, code string) error {
	message, stack, _ := strings.Cut(text, "\n    at ")
	message = strings.TrimRight(message, "\n")
	// The error is the first line of the tool result; a message with line
	// breaks could forge the lines after it.
	message = scriptLineBreaks.Replace(message)
	if len(message) > maxScriptErrorBytes {
		message = scriptCut(message, maxScriptErrorBytes) + " [truncated]"
	}
	line := scriptErrorLine(strings.Split("at "+stack, "\n"), strings.Count(code, "\n")+1)
	if line > 0 {
		message += " (line " + strconv.Itoa(line) + ")"
	}
	return ez.New(ez.EINVALID, "mcp: script error: "+message, nil)
}

// scriptErrorLine is the line of the agent's code in the first frame of the
// stack that names one, or 0. Frames on the two lines new Function adds
// before the code belong to code the script compiled itself, so they are
// skipped; a syntax error at the end of the input, which the parser
// reports on the wrapper's closing line, is clamped to the last line of
// the code.
func scriptErrorLine(frames []string, codeLines int) int {
	for i, frame := range frames {
		match := scriptLine.FindStringSubmatch(frame)
		if match == nil {
			continue
		}
		if i+1 < len(frames) && strings.TrimSpace(frames[i+1]) == scriptJSONFrame {
			continue
		}
		line, err := strconv.Atoi(match[1])
		if err != nil || line <= scriptCodeLines {
			continue
		}
		return min(line-scriptCodeLines, codeLines)
	}
	return 0
}

// scriptLimiter bounds the scripts running at once: MaxConcurrent across all
// callers and one per caller.
type scriptLimiter struct {
	slots   chan struct{}
	mu      sync.Mutex
	running map[string]bool
}

func newScriptLimiter(maxConcurrent int) *scriptLimiter {
	if maxConcurrent <= 0 {
		maxConcurrent = defaultScriptMaxConcurrent
	}
	return &scriptLimiter{slots: make(chan struct{}, maxConcurrent), running: map[string]bool{}}
}

// slot takes one of the MaxConcurrent slots. It refuses after
// scriptSlotWait when every slot stays busy. Call the returned function when
// the script ends.
func (l *scriptLimiter) slot(ctx context.Context) (func(), error) {
	timer := time.NewTimer(scriptSlotWait)
	defer timer.Stop()
	select {
	case l.slots <- struct{}{}:
		return func() {
			<-l.slots
		}, nil
	case <-timer.C:
		return nil, ez.New(ez.ERESOURCEEXHAUSTED, "mcp: too many scripts are running; try again shortly", nil)
	case <-ctx.Done():
		return nil, ez.New(ez.ECANCELED, fmt.Sprintf("mcp: script canceled: %v", ctx.Err()), nil)
	}
}

// claim marks the caller identified by key as running a script. It refuses
// at once when the caller already runs one. Call the returned function when
// the script ends.
func (l *scriptLimiter) claim(key string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[key] {
		return nil, ez.New(ez.ERESOURCEEXHAUSTED, "mcp: a script is already running for this account; wait for it to finish", nil)
	}
	l.running[key] = true
	return func() {
		l.mu.Lock()
		delete(l.running, key)
		l.mu.Unlock()
	}, nil
}
