// Package models has a comment no key records.
package models

// Run is one execution of a job.
//
// Deprecated: use Job.
type Run struct {
	Base
	// Extra is what a run may carry.
	*Extra
	// Name is what the user calls the run.
	Name string `json:"name"` // It is unique.
	// TODO: rename to Title.
	// Title is shown in lists.
	Title string
	// Old is kept for old clients.
	//
	// Deprecated: use Name.
	Old string
	// First and Second share one comment.
	First, Second int
	// secret is not sent.
	secret string
	Plain  string
}

type (
	// Step is one step of a run.
	Step struct {
		// Order is where the step runs.
		Order int
	}

	// Kind is a named non-struct type.
	Kind string
)

// Pair is declared with another type, so this comment describes neither.
type (
	Left  struct{}
	Right struct{}
)

// Job is a run under another name.
type Job Run

type Task Job

// RunPage is a page of runs.
type RunPage Page[Run]

// Alias is an alias, which has no entry.
type Alias = Run

// Page is one page of a list.
type Page[T any] struct {
	// Items are the entries of the page.
	Items []T
}

// base is unexported, but its exported fields are promoted.
type base struct {
	// ID identifies the record.
	ID string
}

// Base is exported.
type Base = base

// Extra is embedded through a pointer.
type Extra struct {
	// Meta is a nested struct without a name.
	Meta struct {
		// Inner has no key: its struct is anonymous.
		Inner string
	}
}

// FIXME: only a maintainer note.
type Noted struct{}

func local() {
	// Local is declared in a function.
	type Local struct {
		// Field of a local type.
		Field string
	}
	_ = Local{}
}
