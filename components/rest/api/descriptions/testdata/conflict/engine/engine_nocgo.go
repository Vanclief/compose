//go:build !cgo

package engine

// Engine runs without cgo.
type Engine struct {
	// Name names the engine.
	Name string
}
