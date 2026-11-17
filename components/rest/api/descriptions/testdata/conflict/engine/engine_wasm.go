//go:build wasm

package engine

// Engine runs with cgo.
type Engine struct {
	// Name names the engine.
	Name string
}
