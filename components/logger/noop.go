package logger

import "time"

type (
	Noop      struct{} // zero-size, safe to copy
	noopEvent struct{} // zero-size, safe to reuse
)

// With returns itself
func (Noop) With(_ ...any) Logger { return Noop{} }

func (Noop) Debug() Event { return noopEvent{} }
func (Noop) Info() Event  { return noopEvent{} }
func (Noop) Warn() Event  { return noopEvent{} }
func (Noop) Error() Event { return noopEvent{} }

// All builder methods are no-ops returning the same event.
func (noopEvent) Str(string, string) Event        { return noopEvent{} }
func (noopEvent) Int(string, int) Event           { return noopEvent{} }
func (noopEvent) Int64(string, int64) Event       { return noopEvent{} }
func (noopEvent) Uint64(string, uint64) Event     { return noopEvent{} }
func (noopEvent) Float64(string, float64) Event   { return noopEvent{} }
func (noopEvent) Bool(string, bool) Event         { return noopEvent{} }
func (noopEvent) Time(string, time.Time) Event    { return noopEvent{} }
func (noopEvent) Dur(string, time.Duration) Event { return noopEvent{} }
func (noopEvent) Err(error) Event                 { return noopEvent{} }
func (noopEvent) Bytes(string, []byte) Event      { return noopEvent{} }
func (noopEvent) Any(string, any) Event           { return noopEvent{} }
func (noopEvent) Msg(string)                      { /* no-op */ }

var (
	_ Logger = Noop{}
	_ Event  = noopEvent{}
)
