// Package fixture mirrors auditor3's runs module for api registry tests.
package fixture

import (
	"context"
	"io"
	"mime/multipart"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/requests"
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

// EditRequest is a partial update whose fields tell absent, null and a
// value apart.
type EditRequest struct {
	requests.NoRules
	RunID  uuid.UUID                 `json:"-" path:"id"`
	Name   types.Optional[string]    `json:"name" minLength:"1"`
	Status types.Optional[RunStatus] `json:"status"`
	Config types.Optional[Config]    `json:"config"`
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

// UploadRequest is a multipart/form-data upload: path param, form values
// and files.
type UploadRequest struct {
	requests.NoRules
	RunID    uuid.UUID               `json:"-" path:"id"`
	Category string                  `json:"category" required:"true" enum:"log,report"`
	Attempt  *int                    `json:"attempt,omitempty" minimum:"1"`
	Tags     []string                `json:"tags,omitempty"`
	Manifest *multipart.FileHeader   `json:"-" form:"manifest" required:"true" doc:"Run manifest"`
	Files    []*multipart.FileHeader `json:"-" form:"files"`
}

// UploadResponse echoes what the App received.
type UploadResponse struct {
	Manifest string   `json:"manifest"`
	Files    []string `json:"files"`
}

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

// App is a fake app: its typed handlers record the body they received
// (after the registry ran Validate, which may normalize it) and the
// negotiated media type, and return canned responses.
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

// Authenticate accepts every request.
func (a *App) Authenticate(request requests.Request) error {
	return nil
}

// record stores body and the media type negotiated for ctx.
func (a *App) record(ctx context.Context, body requests.Body) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.received = append(a.received, body)
	a.accept = api.Accept(ctx)
}

// List handles ListRequest.
func (a *App) List(ctx context.Context, body *ListRequest) (ListResponse, error) {
	a.record(ctx, body)
	return ListResponse{Runs: []Run{sampleRun(KnownID)}}, nil
}

// Get handles GetRequest; MissingID is not found.
func (a *App) Get(ctx context.Context, body *GetRequest) (Run, error) {
	a.record(ctx, body)
	if body.RunID == MissingID {
		return Run{}, ez.New(ez.ENOTFOUND, "run not found", nil)
	}
	return sampleRun(body.RunID), nil
}

// Create handles CreateRequest.
func (a *App) Create(ctx context.Context, body *CreateRequest) (Run, error) {
	a.record(ctx, body)
	run := sampleRun(KnownID)
	run.Name = body.Name
	return run, nil
}

// Update handles UpdateRequest.
func (a *App) Update(ctx context.Context, body *UpdateRequest) (Run, error) {
	a.record(ctx, body)
	run := sampleRun(body.RunID)
	run.FailureReason = body.FailureReason
	return run, nil
}

// Edit handles EditRequest.
func (a *App) Edit(ctx context.Context, body *EditRequest) (Run, error) {
	a.record(ctx, body)
	run := sampleRun(body.RunID)
	name, ok := body.Name.Get()
	if ok {
		run.Name = name
	}
	return run, nil
}

// Archive handles ArchiveRequest.
func (a *App) Archive(ctx context.Context, body *ArchiveRequest) (Run, error) {
	a.record(ctx, body)
	return sampleRun(KnownID), nil
}

// Report returns the run report as a PDF; MissingID is not found.
func (a *App) Report(ctx context.Context, body *ReportRequest) (api.Blob, error) {
	a.record(ctx, body)
	if body.RunID == MissingID {
		return api.Blob{}, ez.New(ez.ENOTFOUND, "run not found", nil)
	}
	return api.Blob{ContentType: "application/pdf", Body: []byte("%PDF-1.7 " + body.RunID.String()), Filename: "run report.pdf"}, nil
}

// Export lists runs as JSON.
func (a *App) Export(ctx context.Context, body *ExportRequest) (ListResponse, error) {
	a.record(ctx, body)
	return ListResponse{Runs: []Run{sampleRun(KnownID)}}, nil
}

// ExportCSV lists runs as CSV, without a ContentType so the negotiated one
// is written.
func (a *App) ExportCSV(ctx context.Context, body *ExportRequest) (api.Blob, error) {
	a.record(ctx, body)
	return api.Blob{Body: []byte(ExportCSV), Filename: "runs.csv"}, nil
}

// Upload handles UploadRequest.
func (a *App) Upload(ctx context.Context, body *UploadRequest) (UploadResponse, error) {
	a.record(ctx, body)
	return upload(body)
}

// Health handles HealthRequest.
func (a *App) Health(ctx context.Context, body *HealthRequest) (HealthResponse, error) {
	a.record(ctx, body)
	return HealthResponse{OK: true}, nil
}

// upload reads the manifest and lists the other files' names.
func upload(r *UploadRequest) (UploadResponse, error) {
	f, err := r.Manifest.Open()
	if err != nil {
		return UploadResponse{}, ez.Wrap(err)
	}
	defer f.Close()

	manifest, err := io.ReadAll(f)
	if err != nil {
		return UploadResponse{}, ez.Wrap(err)
	}

	resp := UploadResponse{Manifest: string(manifest), Files: []string{}}
	for _, file := range r.Files {
		resp.Files = append(resp.Files, file.Filename)
	}
	return resp, nil
}

func sampleRun(id uuid.UUID) Run {
	return Run{
		ID:        id,
		Name:      "nightly",
		Status:    RunningStatus,
		CreatedAt: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC),
	}
}
