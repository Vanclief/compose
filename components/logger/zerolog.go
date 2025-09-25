package logger

import (
	"encoding/json"
	"fmt"
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
		switch v := kv[i+1].(type) {
		case time.Time:
			ctx = ctx.Time(k, v)
		case time.Duration:
			ctx = ctx.Dur(k, v)
		case error:
			if v != nil {
				ctx = ctx.AnErr(k, v)
			}
		case string:
			ctx = ctx.Str(k, v)
		case bool:
			ctx = ctx.Bool(k, v)
		case int:
			ctx = ctx.Int(k, v)
		case int64:
			ctx = ctx.Int64(k, v)
		case uint64:
			ctx = ctx.Uint64(k, v)
		case float64:
			ctx = ctx.Float64(k, v)
		case fmt.Stringer:
			ctx = ctx.Str(k, v.String())
		default:
			ctx = ctx.Interface(k, v)
		}
	}
	return &zeroLogger{l: ctx.Logger()}
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
