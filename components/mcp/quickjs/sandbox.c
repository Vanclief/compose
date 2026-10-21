/*
 * sandbox.c is the guest side of run_script: QuickJS (quickjs-ng, without
 * quickjs-libc) behind one entry point. script.go in the parent directory is
 * the host side. Every pointer and length is an i32 into linear memory, and
 * every text is UTF-8 passed with its length, so a NUL inside one survives.
 *
 * Exports:
 *
 *   malloc(size) -> ptr, free(ptr)
 *       wasi-libc's allocator. The host writes its inputs and the replies of
 *       host_call into memory it takes with malloc.
 *
 *   run(memory_limit, stack_limit, shim_ptr, shim_len, code_ptr, code_len,
 *       out_ptr_ptr, out_len_ptr) -> kind
 *       Called once per instance. It creates a runtime whose allocator
 *       refuses to go past memory_limit bytes and whose stack limit is
 *       stack_limit bytes, 0 for none in both cases, and a context,
 *       installs __call and __log on the global object, and evaluates the
 *       shim (followed by a NUL, which JS_Eval needs) as shim.js. The shim
 *       must evaluate to an object with three functions:
 *         run(code)      compiles code with new Function and calls it,
 *         encode(value)  returns the JSON text of value, always a string,
 *         describe(e)    returns the text of a thrown value, always a string.
 *       run passes the code to shim.run as a string argument and never
 *       splices it into source the shim evaluates. new Function compiles
 *       it at global scope, so code that closes its function body early
 *       only reaches global scope, where the shim keeps nothing it depends
 *       on. It then runs
 *       the pending jobs, settles a returned promise (fulfilled: its value;
 *       rejected: its reason is thrown; pending after the jobs ran: a
 *       TypeError) and encodes the value. It stores a malloc'd copy of a
 *       text at *out_ptr_ptr, its length at *out_len_ptr, frees the context
 *       and the runtime and returns the kind of the text:
 *         0 RESULT  the JSON of the result. When the copy cannot be
 *                   allocated the pointer is 0 and the length is still set.
 *         1 THROWN  describe() of the thrown value, or a fixed text when
 *                   that fails or the result cannot be read.
 *         2 FAILED  the glue failed: no runtime, or the shim threw.
 *       The linker places a 1 MiB stack at the bottom of linear memory. With
 *       a stack_limit below it, recursion that reaches the limit throws a
 *       catchable RangeError; native code that recurses past the last check
 *       runs below address 0 and traps instead of overwriting data.
 *
 * Imports, module "env":
 *
 *   host_call(name_ptr, name_len, args_ptr, args_len, reply_len_ptr) -> reply_ptr
 *       Calls a catalog function. The host allocates the reply with malloc,
 *       writes its length at reply_len_ptr and returns its address; this
 *       side frees it. 0 means the host could not allocate it: the call
 *       throws "InternalError: the script ran out of memory".
 *
 *   host_log(ptr, len)
 *       Records one console.log line.
 *
 * The JavaScript side sees __call(name, args) and __log(line), which only
 * accept strings: converting another value would run script code in the
 * middle of a host call. The shim takes both off the global object before
 * the agent's code runs.
 */
#include <malloc.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "quickjs.h"

#define IMPORT(name) __attribute__((import_module("env"), import_name(name)))
#define EXPORT(name) __attribute__((export_name(name)))

IMPORT("host_call")
char *host_call(const char *name, size_t name_len, const char *args, size_t args_len, size_t *reply_len);

IMPORT("host_log")
void host_log(const char *line, size_t len);

enum { RUN_RESULT = 0, RUN_THROWN = 1, RUN_FAILED = 2 };

static const char text_undescribable[] = "a thrown value that cannot be converted to text";
static const char text_out_of_memory[] = "InternalError: out of memory";
static const char text_no_runtime[] = "the runtime cannot be created";

static JSValue js_host_call(JSContext *ctx, JSValueConst this_val, int argc, JSValueConst *argv)
{
    const char *name, *args;
    size_t name_len, args_len, reply_len = 0;
    char *reply;
    JSValue value;

    if (argc < 2 || !JS_IsString(argv[0]) || !JS_IsString(argv[1]))
        return JS_ThrowTypeError(ctx, "__call takes two strings");
    name = JS_ToCStringLen(ctx, &name_len, argv[0]);
    if (!name)
        return JS_EXCEPTION;
    args = JS_ToCStringLen(ctx, &args_len, argv[1]);
    if (!args) {
        JS_FreeCString(ctx, name);
        return JS_EXCEPTION;
    }
    reply = host_call(name, name_len, args, args_len, &reply_len);
    JS_FreeCString(ctx, name);
    JS_FreeCString(ctx, args);
    /* From here the host ran: a reply that cannot be read is undefined,
     * which the shim tells apart from an exception raised before the call. */
    if (!reply)
        return JS_UNDEFINED;
    value = JS_NewStringLen(ctx, reply, reply_len);
    free(reply);
    if (JS_IsException(value)) {
        JS_FreeValue(ctx, JS_GetException(ctx));
        return JS_UNDEFINED;
    }
    return value;
}

static JSValue js_host_log(JSContext *ctx, JSValueConst this_val, int argc, JSValueConst *argv)
{
    const char *line;
    size_t len;

    if (argc < 1 || !JS_IsString(argv[0]))
        return JS_UNDEFINED;
    line = JS_ToCStringLen(ctx, &len, argv[0]);
    if (!line)
        return JS_EXCEPTION;
    host_log(line, len);
    JS_FreeCString(ctx, line);
    return JS_UNDEFINED;
}

/* call_shim calls shim[name](arg) and frees arg. */
static JSValue call_shim(JSContext *ctx, JSValueConst shim, const char *name, JSValue arg)
{
    JSValue fn, value;

    if (JS_IsException(arg))
        return JS_EXCEPTION;
    fn = JS_GetPropertyStr(ctx, shim, name);
    if (JS_IsException(fn)) {
        JS_FreeValue(ctx, arg);
        return JS_EXCEPTION;
    }
    value = JS_Call(ctx, fn, JS_UNDEFINED, 1, (JSValueConst *)&arg);
    JS_FreeValue(ctx, fn);
    JS_FreeValue(ctx, arg);
    return value;
}

/* drain runs the pending jobs; on a job that throws, the exception stays
 * pending and drain returns -1. */
static int drain(JSRuntime *rt)
{
    JSContext *job_ctx;
    int ret;

    for (;;) {
        ret = JS_ExecutePendingJob(rt, &job_ctx);
        if (ret <= 0)
            return ret;
    }
}

/* settle replaces a promise with its value, or throws its reason. It takes
 * ownership of value. */
static JSValue settle(JSContext *ctx, JSValue value)
{
    JSValue result;

    switch (JS_PromiseState(ctx, value)) {
    case JS_PROMISE_FULFILLED:
        result = JS_PromiseResult(ctx, value);
        JS_FreeValue(ctx, value);
        return result;
    case JS_PROMISE_REJECTED:
        result = JS_PromiseResult(ctx, value);
        JS_FreeValue(ctx, value);
        return JS_Throw(ctx, result);
    case JS_PROMISE_PENDING:
        JS_FreeValue(ctx, value);
        return JS_ThrowTypeError(ctx, "the script returned a promise that never settles");
    default:
        return value;
    }
}

/* copy stores a malloc'd copy of text in *out. */
static int copy(int kind, const char *text, size_t len, char **out, size_t *out_len)
{
    char *buf = malloc(len + 1);

    if (buf) {
        memcpy(buf, text, len);
        buf[len] = 0;
    }
    *out = buf;
    *out_len = len;
    return kind;
}

/* finish stores the string str, which it frees, as a text of kind kind, or
 * fallback as a text of kind fallback_kind when str is not a string or
 * cannot be read. */
static int finish(JSContext *ctx, int kind, JSValue str, int fallback_kind, const char *fallback, char **out, size_t *out_len)
{
    const char *text = NULL;
    size_t len = 0;
    int ret;

    if (JS_IsString(str))
        text = JS_ToCStringLen(ctx, &len, str);
    JS_FreeValue(ctx, str);
    if (!text) {
        JS_FreeValue(ctx, JS_GetException(ctx));
        return copy(fallback_kind, fallback, strlen(fallback), out, out_len);
    }
    ret = copy(kind, text, len, out, out_len);
    JS_FreeCString(ctx, text);
    return ret;
}

/*
 * The allocator of the runtime, with the memory limit. QuickJS checks its
 * own limit before calling the allocator, so a refusal there leaves no
 * trace, and when it cannot build the out-of-memory error it throws null,
 * or an InternalError whose message is "Invalid error message". This
 * allocator enforces the limit itself, and after a refusal allows
 * HEAP_GRACE above what is used, so QuickJS can build the error even when
 * an earlier refusal was swallowed, such as a backtrace that did not fit;
 * the grace stops at HEAP_GRACE_MAX over the limit, and ends once frees
 * bring the heap HEAP_SETTLE under the limit, so a script cannot ratchet
 * the limit up. thrown() reports the two fallbacks as out of memory when
 * the allocator refused. A script that catches an out-of-memory error and
 * later throws null on purpose is reported as out of memory too.
 */
#define HEAP_GRACE (64 * 1024)
#define HEAP_GRACE_MAX (512 * 1024)

/* HEAP_SETTLE is the room under the limit that ends the grace: enough to
 * build an error, so a free during its construction cannot end the grace
 * and refuse the next allocation. */
#define HEAP_SETTLE (16 * 1024)

/* allowed is the limit plus the grace in force; exhausted stays set once
 * the allocator refused, for thrown(), and refused is cleared by thrown()
 * before it describes the exception. */
struct heap {
    size_t used;
    size_t limit;
    size_t allowed;
    int exhausted;
    int refused;
};

/* heap_room is what is still allowed. malloc rounds a request up by a few
 * bytes, so used can pass allowed; the room then saturates at 0 instead
 * of wrapping. */
static size_t heap_room(const struct heap *h)
{
    return h->used < h->allowed ? h->allowed - h->used : 0;
}

/* heap_settle ends the grace once the heap is HEAP_SETTLE under the limit. */
static void heap_settle(struct heap *h)
{
    if (h->used + HEAP_SETTLE < h->limit)
        h->allowed = h->limit;
}

static void *heap_refuse(struct heap *h)
{
    size_t grace;

    h->exhausted = 1;
    h->refused = 1;
    if (h->limit > SIZE_MAX - HEAP_GRACE_MAX || h->used > SIZE_MAX - HEAP_GRACE)
        return NULL;
    grace = h->used + HEAP_GRACE;
    if (grace > h->limit + HEAP_GRACE_MAX)
        grace = h->limit + HEAP_GRACE_MAX;
    if (grace > h->allowed)
        h->allowed = grace;
    return NULL;
}

static void *heap_malloc(void *opaque, size_t size)
{
    struct heap *h = opaque;
    void *ptr;
    size_t usable;

    if (size > heap_room(h))
        return heap_refuse(h);
    ptr = malloc(size);
    if (!ptr)
        return heap_refuse(h);
    usable = malloc_usable_size(ptr);
    if (usable > heap_room(h)) {
        free(ptr);
        return heap_refuse(h);
    }
    h->used += usable;
    return ptr;
}

static void *heap_calloc(void *opaque, size_t count, size_t size)
{
    struct heap *h = opaque;
    void *ptr;

    if (size && count > SIZE_MAX / size)
        return heap_refuse(h);
    ptr = heap_malloc(opaque, count * size);
    if (ptr)
        memset(ptr, 0, count * size);
    return ptr;
}

static void heap_free(void *opaque, void *ptr)
{
    struct heap *h = opaque;

    if (!ptr)
        return;
    h->used -= malloc_usable_size(ptr);
    free(ptr);
    heap_settle(h);
}

static void *heap_realloc(void *opaque, void *ptr, size_t size)
{
    struct heap *h = opaque;
    size_t old;
    void *next;

    if (!ptr)
        return heap_malloc(opaque, size);
    if (size == 0) {
        heap_free(opaque, ptr);
        return NULL;
    }
    old = malloc_usable_size(ptr);
    if (size > old && size - old > heap_room(h))
        return heap_refuse(h);
    next = realloc(ptr, size);
    if (!next)
        return heap_refuse(h);
    h->used = h->used - old + malloc_usable_size(next);
    heap_settle(h);
    return next;
}

static size_t heap_usable_size(const void *ptr)
{
    return malloc_usable_size((void *)ptr);
}

static const JSMallocFunctions heap_functions = {
    heap_calloc,
    heap_malloc,
    heap_free,
    heap_realloc,
    heap_usable_size,
};

/* invalid_error_message is whether exception is an Error whose message is
 * the text QuickJS uses when it cannot allocate the real one. A getter the
 * script put on message runs here and again in describe. */
static int invalid_error_message(JSContext *ctx, JSValueConst exception)
{
    JSValue message;
    const char *text;
    int invalid = 0;

    if (!JS_IsError(exception))
        return 0;
    message = JS_GetPropertyStr(ctx, exception, "message");
    if (JS_IsException(message)) {
        JS_FreeValue(ctx, JS_GetException(ctx));
        return 0;
    }
    if (JS_IsString(message)) {
        text = JS_ToCString(ctx, message);
        if (text) {
            invalid = strcmp(text, "Invalid error message") == 0;
            JS_FreeCString(ctx, text);
        } else {
            JS_FreeValue(ctx, JS_GetException(ctx));
        }
    }
    JS_FreeValue(ctx, message);
    return invalid;
}

/* thrown describes the pending exception. */
static int thrown(JSContext *ctx, JSValueConst shim, struct heap *heap, char **out, size_t *out_len)
{
    JSValue exception = JS_GetException(ctx);
    JSValue text;

    if (heap->exhausted && (JS_IsNull(exception) || invalid_error_message(ctx, exception))) {
        JS_FreeValue(ctx, exception);
        return copy(RUN_THROWN, text_out_of_memory, strlen(text_out_of_memory), out, out_len);
    }
    heap->refused = 0;
    text = call_shim(ctx, shim, "describe", exception);
    if (JS_IsException(text) && heap->refused) {
        JS_FreeValue(ctx, JS_GetException(ctx));
        return copy(RUN_THROWN, text_out_of_memory, strlen(text_out_of_memory), out, out_len);
    }
    return finish(ctx, RUN_THROWN, text, RUN_THROWN, text_undescribable, out, out_len);
}

EXPORT("run")
int run(size_t memory_limit, size_t stack_limit, const char *shim_src, size_t shim_len, const char *code, size_t code_len, char **out, size_t *out_len)
{
    JSRuntime *rt;
    JSContext *ctx;
    JSValue global, shim, value;
    size_t limit = memory_limit ? memory_limit : SIZE_MAX;
    struct heap heap = {0, limit, limit, 0, 0};
    int kind;

    *out = NULL;
    *out_len = 0;
    rt = JS_NewRuntime2(&heap_functions, &heap);
    if (!rt)
        return copy(RUN_FAILED, text_no_runtime, strlen(text_no_runtime), out, out_len);
    JS_SetMaxStackSize(rt, stack_limit);
    ctx = JS_NewContext(rt);
    if (!ctx) {
        JS_FreeRuntime(rt);
        return copy(RUN_FAILED, text_no_runtime, strlen(text_no_runtime), out, out_len);
    }

    global = JS_GetGlobalObject(ctx);
    JS_SetPropertyStr(ctx, global, "__call", JS_NewCFunction(ctx, js_host_call, "__call", 2));
    JS_SetPropertyStr(ctx, global, "__log", JS_NewCFunction(ctx, js_host_log, "__log", 1));
    JS_FreeValue(ctx, global);

    shim = JS_Eval(ctx, shim_src, shim_len, "shim.js", JS_EVAL_TYPE_GLOBAL);
    if (JS_IsException(shim)) {
        value = JS_GetException(ctx);
        kind = finish(ctx, RUN_FAILED, JS_ToString(ctx, value), RUN_FAILED, "the shim failed", out, out_len);
        JS_FreeValue(ctx, value);
        goto done;
    }

    value = call_shim(ctx, shim, "run", JS_NewStringLen(ctx, code, code_len));
    if (!JS_IsException(value) && drain(rt) < 0) {
        JS_FreeValue(ctx, value);
        value = JS_EXCEPTION;
    }
    if (!JS_IsException(value))
        value = settle(ctx, value);
    if (!JS_IsException(value))
        value = call_shim(ctx, shim, "encode", value);
    if (JS_IsException(value))
        kind = thrown(ctx, shim, &heap, out, out_len);
    else
        kind = finish(ctx, RUN_RESULT, value, RUN_THROWN, text_out_of_memory, out, out_len);
    JS_FreeValue(ctx, shim);

done:
    JS_FreeContext(ctx);
    JS_FreeRuntime(rt);
    return kind;
}
