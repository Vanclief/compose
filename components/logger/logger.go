package logger

import "time"

// Logger produces Event builders at specific levels and can attach context fields.
// Implementations MUST be safe for concurrent use.
type Logger interface {
	With(kv ...any) Logger

	Debug() Event
	Info() Event
	Warn() Event
	Error() Event
}

// Event is a one shot builder. Callers add fields, then finish with Msg following Zerolog style
type Event interface {
	// Field helpers
	Str(key, val string) Event
	Int(key string, val int) Event
	Int64(key string, val int64) Event
	Uint64(key string, val uint64) Event
	Float64(key string, val float64) Event
	Bool(key string, val bool) Event
	Time(key string, t time.Time) Event
	Dur(key string, d time.Duration) Event
	Bytes(key string, b []byte) Event
	Err(err error) Event
	Any(key string, v any) Event

	// Finalize
	Msg(msg string)
}
