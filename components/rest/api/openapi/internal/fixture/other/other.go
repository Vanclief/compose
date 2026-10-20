// Package other holds a type whose name collides with fixture.Run, to prove
// package-qualified component names keep them apart.
package other

// Run is unrelated to fixture.Run.
type Run struct {
	Name string `json:"name"`
}
