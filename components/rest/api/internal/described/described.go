// Package described declares the types of the registry tests of
// api.Descriptions. Their comments are what the tests read with
// descriptions.Read and expect in the documents, so change both together.
package described

import (
	"context"
	"mime/multipart"

	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/compose/primitives/enums"
)

// Status is where a run is.
type Status string

const (
	Pending Status = "pending"
	Done    Status = "done"
)

var Statuses = enums.Set([]Status{Pending, Done})

// Record holds what every stored record has.
type Record struct {
	// ID identifies the record.
	ID string `json:"id"`
}

// Owner is the person who started a run.
type Owner struct {
	Email string `json:"email"` // Email reaches the owner.
}

// Stamp says when something happened.
type Stamp struct {
	// At is when it happened.
	At string `json:"at,omitempty"`
}

// Audit says who wrote a record.
type Audit struct {
	Stamp
	// Author wrote the record.
	Author string `json:"author,omitempty"`
}

// Run is one execution of a job.
type Run struct {
	Record
	*Audit
	// Name is what the user calls the run.
	Name string `json:"name"`
	// Label has a comment its doc tag replaces.
	Label string `json:"label" doc:"Label from the tag."`
	// Note has a comment its empty doc tag hides.
	Note string `json:"note" doc:""`
	// Owner started the run.
	Owner Owner `json:"owner"`
	// Status tells where the run is.
	Status Status `json:"status"`
	Plain  string `json:"plain"`
}

// Page is one page of a list.
type Page[T any] struct {
	// Items are the entries of the page.
	Items []T `json:"items"`
}

// Scope names the project of a request.
type Scope struct {
	// Project is the project the run belongs to.
	Project string `json:"-" path:"project"`
}

// CreateRequest starts a run.
type CreateRequest struct {
	requests.NoRules
	Scope
	// Limit caps the steps of the run.
	Limit int `json:"-" query:"limit" minimum:"1"`
	// Trace follows the request across services.
	Trace string `json:"-" header:"x-trace"`
	// Name is what the new run is called.
	Name string `json:"name" required:"true" minLength:"1"`
	// Owner starts the new run.
	Owner *Owner `json:"owner,omitempty"`
}

// ListRequest lists the runs of a project.
type ListRequest struct {
	requests.NoRules
	Scope
}

// UploadRequest attaches a log to a run.
type UploadRequest struct {
	requests.NoRules
	Scope
	// Log is the file to attach.
	Log *multipart.FileHeader `json:"-" form:"log" required:"true"`
	// Extras are more files to attach.
	Extras []*multipart.FileHeader `json:"-" form:"extras"`
	// Comment says what the log shows.
	Comment string `json:"comment"`
}

// ArchiveRequest archives the runs of a project. Only a hidden operation
// takes it.
type ArchiveRequest struct {
	requests.NoRules
	Scope
	// Reason says why the runs are archived.
	Reason string `json:"reason"`
}

// UploadResponse tells what was attached.
type UploadResponse struct {
	// Size is the size of the log in bytes.
	Size int64 `json:"size"`
}

// Create handles CreateRequest.
func Create(ctx context.Context, req *CreateRequest) (Run, error) {
	return Run{Name: req.Name, Status: Pending}, nil
}

// List handles ListRequest.
func List(ctx context.Context, req *ListRequest) (Page[Run], error) {
	return Page[Run]{}, nil
}

// Archive handles ArchiveRequest.
func Archive(ctx context.Context, req *ArchiveRequest) (UploadResponse, error) {
	return UploadResponse{}, nil
}

// Upload handles UploadRequest.
func Upload(ctx context.Context, req *UploadRequest) (UploadResponse, error) {
	return UploadResponse{Size: req.Log.Size}, nil
}
