package logger

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/rs/zerolog"
)

type zeroLogger struct{ l zerolog.Logger }

func NewZero(l zerolog.Logger) Logger { return &zeroLogger{l: l} }

func (z *zeroLogger) With(kv ...any) Logger {
	ctx := z.l.With()
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			continue
		}
		if isNilPointer(kv[i+1]) {
			addNilPointer(&ctx, k, kv[i+1])
		} else {
			addField(&ctx, k, kv[i+1])
		}
	}
	return &zeroLogger{l: ctx.Logger()}
}

func isNilPointer(value any) bool {
	rv := reflect.ValueOf(value)

	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// addNilPointer sets the field k of ctx to a nil pointer. A method called
// through a nil pointer may panic: one with a value receiver always does, and
// one with a pointer receiver does when it reads its receiver without checking
// it, as (*url.URL).String does. Such a panic is caught and the field is
// logged as null, or left out when the value is an error, as a nil error is. A
// method that handles a nil receiver is called and its result logged.
func addNilPointer(ctx *zerolog.Context, k string, value any) {
	ok := tryField(ctx, k, value)
	if ok {
		return
	}

	_, isError := value.(error)
	if isError {
		return
	}

	*ctx = ctx.Interface(k, nil)
}

// tryField sets the field and reports whether that finished. It stops a panic
// from the value's method and reports false, with ctx as it was: zerolog
// returns the context with the field only once the field is written in full.
// Whether there was a panic is told by ok and not by what recover returns,
// which is nil for panic(nil) when the program runs with GODEBUG=panicnil=1.
func tryField(ctx *zerolog.Context, k string, value any) (ok bool) {
	defer func() {
		if !ok {
			recover()
		}
	}()

	addField(ctx, k, value)
	ok = true

	return ok
}

// addField sets the field k of ctx to value, written by the method its type
// calls for. A panic from that method is not caught here.
func addField(ctx *zerolog.Context, k string, value any) {
	switch v := value.(type) {
	case time.Time:
		*ctx = ctx.Time(k, v)
	case time.Duration:
		*ctx = ctx.Dur(k, v)
	case error:
		if v != nil {
			*ctx = ctx.AnErr(k, v)
		}
	case string:
		*ctx = ctx.Str(k, v)
	case bool:
		*ctx = ctx.Bool(k, v)
	case int:
		*ctx = ctx.Int(k, v)
	case int64:
		*ctx = ctx.Int64(k, v)
	case uint64:
		*ctx = ctx.Uint64(k, v)
	case float64:
		*ctx = ctx.Float64(k, v)
	case fmt.Stringer:
		*ctx = ctx.Str(k, v.String())
	default:
		*ctx = ctx.Interface(k, v)
	}
}

func (z *zeroLogger) Debug() Event { return &zeroEvent{e: z.l.Debug()} }
func (z *zeroLogger) Info() Event  { return &zeroEvent{e: z.l.Info()} }
func (z *zeroLogger) Warn() Event  { return &zeroEvent{e: z.l.Warn()} }
func (z *zeroLogger) Error() Event { return &zeroEvent{e: z.l.Error()} }

type zeroEvent struct{ e *zerolog.Event }

func (e *zeroEvent) Str(k, v string) Event               { e.e = e.e.Str(k, v); return e }
func (e *zeroEvent) Int(k string, v int) Event           { e.e = e.e.Int(k, v); return e }
func (e *zeroEvent) Int64(k string, v int64) Event       { e.e = e.e.Int64(k, v); return e }
func (e *zeroEvent) Uint64(k string, v uint64) Event     { e.e = e.e.Uint64(k, v); return e }
func (e *zeroEvent) Float64(k string, v float64) Event   { e.e = e.e.Float64(k, v); return e }
func (e *zeroEvent) Bool(k string, v bool) Event         { e.e = e.e.Bool(k, v); return e }
func (e *zeroEvent) Time(k string, t time.Time) Event    { e.e = e.e.Time(k, t); return e }
func (e *zeroEvent) Dur(k string, d time.Duration) Event { e.e = e.e.Dur(k, d); return e }

func (e *zeroEvent) Bytes(k string, b []byte) Event {
	if len(b) == 0 {
		e.e = e.e.Str(k, "")
		return e
	}
	if json.Valid(b) {
		e.e = e.e.RawJSON(k, b)
		return e
	}
	e.e = e.e.Hex(k, b)

	return e
}

func (e *zeroEvent) Err(err error) Event {
	if err != nil {
		e.e = e.e.Err(err)
	}
	return e
}
func (e *zeroEvent) Any(k string, v any) Event { e.e = e.e.Interface(k, v); return e }
func (e *zeroEvent) Msg(msg string)            { e.e.Msg(msg) }

var (
	_ Logger = (*zeroLogger)(nil)
	_ Event  = (*zeroEvent)(nil)
)
