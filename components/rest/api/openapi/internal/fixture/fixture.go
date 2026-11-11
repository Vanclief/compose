// Package fixture mirrors auditor3's runs module for openapi reflector tests.
package fixture

import (
	"time"

	"github.com/google/uuid"

	"github.com/vanclief/compose/drivers/databases/relational/postgres/pagination"
	"github.com/vanclief/compose/primitives/enums"
	"github.com/vanclief/compose/types"
)

// RunStatus is a named enum registered through enums.Set.
type RunStatus string

const (
	PendingStatus   RunStatus = "pending"
	RunningStatus   RunStatus = "running"
	CompletedStatus RunStatus = "completed"
	FailedStatus    RunStatus = "failed"
	CancelledStatus RunStatus = "cancelled"
)

var RunStatuses = enums.Set([]RunStatus{PendingStatus, RunningStatus, CompletedStatus, FailedStatus, CancelledStatus})

// Meta is only used in responses.
type Meta struct {
	Source string            `json:"source"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Config is used in both directions (Run response and CreateRequest).
type Config struct {
	Retries int         `json:"retries" minimum:"0"`
	Timeout *int        `json:"timeout,omitempty"`
	Notify  types.Email `json:"notify,omitempty"`
}

// Run is the main response type.
type Run struct {
	ID        uuid.UUID `json:"id"`
	Status    RunStatus `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	Metadata  *Meta     `json:"metadata,omitempty"`
	Tags      []string  `json:"tags"`
	Score     *float64  `json:"score,omitempty"`
	Config    Config    `json:"config"`
}

// ListRequest has no body: every field is a parameter.
type ListRequest struct {
	pagination.CursorRequest
	ProjectID uuid.UUID `json:"-" query:"project_id"`
	Status    RunStatus `json:"-" query:"status"`
}

// ListResponse embeds the cursor response.
type ListResponse struct {
	pagination.CursorResponse
	Runs []Run `json:"runs"`
}

// UpdateRequest is a partial update: nothing is required in the body.
type UpdateRequest struct {
	RunID          uuid.UUID  `json:"-" path:"id"`
	MaxTotalRuns   *int       `json:"max_total_runs,omitempty" minimum:"0"`
	CodeSnapshotID *uuid.UUID `json:"code_snapshot_id,omitempty"`
	Status         *RunStatus `json:"status,omitempty" enum:"failed,cancelled"`
	FailureReason  *string    `json:"failure_reason,omitempty"`
}

// CreateRequest requires exactly name.
type CreateRequest struct {
	Name   string `json:"name" required:"true" minLength:"1"`
	Config Config `json:"config"`
}

// Node is recursive and used in both directions (request root and Tree).
type Node struct {
	Name     string  `json:"name" required:"true"`
	Label    *string `json:"label,omitempty"`
	Children []Node  `json:"children,omitempty"`
}

// Tree is a response wrapping Node.
type Tree struct {
	Root Node `json:"root"`
}

// Page is a generic response wrapper.
type Page[T any] struct {
	Items []T `json:"items"`
}
