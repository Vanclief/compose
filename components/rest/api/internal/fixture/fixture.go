// Package fixture mirrors auditor3's runs module for api registry tests.
package fixture

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/compose/drivers/databases/relational/postgres/pagination"
	"github.com/vanclief/compose/primitives/enums"
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

// Run is the main response type.
type Run struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Status        RunStatus `json:"status"`
	FailureReason *string   `json:"failure_reason,omitempty"`
	MaxTotalRuns  int       `json:"max_total_runs"`
	CreatedAt     time.Time `json:"created_at"`
}

// ListRequest has no body: every field is a parameter. It does not embed
// requests.NoRules because CursorRequest already provides Validate (both
// at the same depth would make Validate ambiguous).
type ListRequest struct {
	pagination.CursorRequest
	ProjectID uuid.UUID  `json:"-" query:"project_id"`
	Status    RunStatus  `json:"-" query:"status"`
	At        time.Time  `json:"-" query:"at"`
	Since     *time.Time `json:"-" header:"x-since"`
}

// ListResponse embeds the cursor response.
type ListResponse struct {
	pagination.CursorResponse
	Runs []Run `json:"runs"`
}

// GetRequest has a single path parameter.
type GetRequest struct {
	requests.NoRules
	RunID uuid.UUID `json:"-" path:"id"`
}

// UpdateRequest is a partial update with a cross-field rule.
type UpdateRequest struct {
	RunID          uuid.UUID  `json:"-" path:"id"`
	MaxTotalRuns   *int       `json:"max_total_runs,omitempty" minimum:"0"`
	CodeSnapshotID *uuid.UUID `json:"code_snapshot_id,omitempty"`
	Status         *RunStatus `json:"status,omitempty" enum:"failed,cancelled"`
	FailureReason  *string    `json:"failure_reason,omitempty"`
}

// Validate keeps only cross-field rules. Shape rules live in tags.
func (r *UpdateRequest) Validate() error {
	if r.Status != nil && *r.Status == FailedStatus && r.FailureReason == nil {
		return ez.New(ez.EINVALID, "failure_reason is required when status is failed", nil)
	}
	return nil
}

// CreateRequest requires name and reads a header.
type CreateRequest struct {
	requests.NoRules
	Name   string            `json:"name" required:"true" minLength:"1"`
	Trace  string            `json:"-" header:"x-trace"`
	Config *Config           `json:"config,omitempty"`
	Steps  []Config          `json:"steps,omitempty"`
	Labels map[string]Config `json:"labels,omitempty"`
}

// Config is a nested request object used to test nested key handling.
type Config struct {
	Retries int `json:"retries" minimum:"0"`
}

// ArchiveRequest is only used by a hidden operation, so its component is
// absent from the public spec but still validated.
type ArchiveRequest struct {
	requests.NoRules
	Reason string `json:"reason" required:"true"`
}

// ReportRequest returns a run report as a PDF api.Blob.
type ReportRequest struct {
	requests.NoRules
	RunID uuid.UUID `json:"-" path:"id"`
}

// ExportRequest lists runs as JSON or, when negotiated, as a CSV api.Blob.
type ExportRequest struct {
	requests.NoRules
	Status RunStatus `json:"-" query:"status"`
}

// ExportCSV is the CSV body the App returns for ExportRequest.
const ExportCSV = "id,name\n0192f3a1-7b2c-7def-8a12-3456789abcde,nightly\n"

// HealthRequest is used by the public operation.
type HealthRequest struct {
	requests.NoRules
}

// HealthResponse is the public operation's response.
type HealthResponse struct {
	OK bool `json:"ok"`
}

// MissingID is the run id the App reports as not found.
var MissingID = uuid.MustParse("01890a5d-ac96-774b-bcce-b302099a8057")

// KnownID is a v7 run id the App returns.
var KnownID = uuid.MustParse("0192f3a1-7b2c-7def-8a12-3456789abcde")

// App is a fake compose App: it records the received body (before calling
// Validate, which may mutate it), validates like the real apps and returns
// canned responses.
type App struct {
	mu       sync.Mutex
	received []requests.Body
	accept   string
}

// Last returns the last received body.
func (a *App) Last() requests.Body {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.received) == 0 {
		return nil
	}
	return a.received[len(a.received)-1]
}

// LastAccept returns the media type negotiated for the last request.
func (a *App) LastAccept() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accept
}

// HandleRequest implements handler.App.
func (a *App) HandleRequest(request requests.Request) (any, error) {
	body := request.GetBody()
	a.record(body)
	a.mu.Lock()
	a.accept = request.GetAccept()
	a.mu.Unlock()

	err := body.Validate()
	if err != nil {
		return nil, ez.Wrap(err)
	}

	switch b := body.(type) {
	case *ListRequest:
		return ListResponse{Runs: []Run{sampleRun(KnownID)}}, nil
	case *GetRequest:
		if b.RunID == MissingID {
			return nil, ez.New(ez.ENOTFOUND, "run not found", nil)
		}
		return sampleRun(b.RunID), nil
	case *CreateRequest:
		run := sampleRun(KnownID)
		run.Name = b.Name
		return &run, nil
	case *UpdateRequest:
		run := sampleRun(b.RunID)
		run.FailureReason = b.FailureReason
		return run, nil
	case *ArchiveRequest:
		return sampleRun(KnownID), nil
	case *ReportRequest:
		if b.RunID == MissingID {
			return nil, ez.New(ez.ENOTFOUND, "run not found", nil)
		}
		return api.Blob{ContentType: "application/pdf", Body: []byte("%PDF-1.7 " + b.RunID.String()), Filename: "run report.pdf"}, nil
	case *ExportRequest:
		if request.GetAccept() == "text/csv" {
			return &api.Blob{Body: []byte(ExportCSV), Filename: "runs.csv"}, nil
		}
		return ListResponse{Runs: []Run{sampleRun(KnownID)}}, nil
	case *HealthRequest:
		return HealthResponse{OK: true}, nil
	}
	return nil, ez.New(ez.EINTERNAL, "unexpected request body", nil)
}

// record stores a shallow copy of body.
func (a *App) record(body requests.Body) {
	var stored requests.Body = body
	switch b := body.(type) {
	case *ListRequest:
		c := *b
		stored = &c
	case *UpdateRequest:
		c := *b
		stored = &c
	}

	a.mu.Lock()
	a.received = append(a.received, stored)
	a.mu.Unlock()
}

func sampleRun(id uuid.UUID) Run {
	return Run{
		ID:        id,
		Name:      "nightly",
		Status:    RunningStatus,
		CreatedAt: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC),
	}
}
