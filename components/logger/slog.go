package logger

import (
	"context"
	"encoding/hex"
	"log/slog"
	"time"
)

type slogLogger struct {
	l *slog.Logger
}

func NewSlog(l *slog.Logger) Logger { return &slogLogger{l: l} }

func (s *slogLogger) With(kv ...any) Logger {
	return &slogLogger{l: s.l.With(kv...)}
}

func (s *slogLogger) Debug() Event { return &slogEvent{l: s.l, level: slog.LevelDebug} }
func (s *slogLogger) Info() Event  { return &slogEvent{l: s.l, level: slog.LevelInfo} }
func (s *slogLogger) Warn() Event  { return &slogEvent{l: s.l, level: slog.LevelWarn} }
func (s *slogLogger) Error() Event { return &slogEvent{l: s.l, level: slog.LevelError} }

type slogEvent struct {
	l     *slog.Logger
	level slog.Level
	attrs []slog.Attr
}

func (e *slogEvent) add(a slog.Attr) *slogEvent          { e.attrs = append(e.attrs, a); return e }
func (e *slogEvent) Str(k, v string) Event               { return e.add(slog.String(k, v)) }
func (e *slogEvent) Int(k string, v int) Event           { return e.add(slog.Int(k, v)) }
func (e *slogEvent) Int64(k string, v int64) Event       { return e.add(slog.Int64(k, v)) }
func (e *slogEvent) Uint64(k string, v uint64) Event     { return e.add(slog.Uint64(k, v)) }
func (e *slogEvent) Float64(k string, v float64) Event   { return e.add(slog.Float64(k, v)) }
func (e *slogEvent) Bool(k string, v bool) Event         { return e.add(slog.Bool(k, v)) }
func (e *slogEvent) Time(k string, t time.Time) Event    { return e.add(slog.Time(k, t)) }
func (e *slogEvent) Dur(k string, d time.Duration) Event { return e.add(slog.Duration(k, d)) }
func (e *slogEvent) Bytes(k string, b []byte) Event {
	if len(b) == 0 {
		return e.add(slog.String(k, "")) // or slog.Any(k, []byte{})
	}
	hexed := hex.EncodeToString(b)
	return e.add(slog.String(k, hexed))
}

func (e *slogEvent) Err(err error) Event {
	if err != nil {
		e.attrs = append(e.attrs, slog.Any("error", err))
	}
	return e
}
func (e *slogEvent) Any(k string, v any) Event { return e.add(slog.Any(k, v)) }
func (e *slogEvent) Msg(msg string)            { e.l.LogAttrs(context.Background(), e.level, msg, e.attrs...) }

var (
	_ Logger = (*slogLogger)(nil)
	_ Event  = (*slogEvent)(nil)
)
