// Package service is what API handlers and daemon-pool nodes call on each
// component. Implementations are wired in cmd/vidyomd (see the master plan,
// section 9). Authorization by route permission happens in the API layer; a
// service also enforces what depends on the resource, such as D7's approval
// rules. Every mutation writes the audit (§12.1).
package service

import (
	"context"
	"io"
	"net/http"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
)

// Scope is the authenticated, tenant-resolved context of one request.
type Scope struct {
	Tenant         domain.TenantID
	Principal      domain.Principal
	RequestID      string
	IdempotencyKey string
	TraceID        string
}

// Workflows is implemented by component engine.
type Workflows interface {
	List(ctx context.Context, s Scope) ([]api.WorkflowSummary, error)
	Get(ctx context.Context, s Scope, workflowID, version string) (api.WorkflowDetail, error)
	Validate(ctx context.Context, s Scope, req api.ValidateWorkflowRequest) (api.ValidationResult, error)
	// Run submits a registered workflow, or req.Definition when workflowID is empty.
	Run(ctx context.Context, s Scope, workflowID string, req api.RunWorkflowRequest) (api.RunWorkflowResponse, error)
	// RunNode is `vidyom node run`: an ad-hoc single-node job.
	RunNode(ctx context.Context, s Scope, nodeType string, req api.RunNodeRequest) (api.RunWorkflowResponse, error)
	// CreateClip runs create-clip from a clip request.
	CreateClip(ctx context.Context, s Scope, req api.CreateClipRequest) (api.RunWorkflowResponse, error)
}

// Jobs is implemented by component engine. Reads never change state; Ctrl+C on a
// stream only drops the stream (§5.5).
type Jobs interface {
	List(ctx context.Context, s Scope, q api.JobQuery) (api.List[domain.Job], error)
	Get(ctx context.Context, s Scope, ref id.Ref) (api.JobDetail, error)
	Cancel(ctx context.Context, s Scope, ref id.Ref, req api.JobActionRequest) (domain.Job, error)
	Retry(ctx context.Context, s Scope, ref id.Ref, req api.JobActionRequest) (domain.Job, error)
	Pause(ctx context.Context, s Scope, ref id.Ref, req api.JobActionRequest) (domain.Job, error)
	Resume(ctx context.Context, s Scope, ref id.Ref, req api.JobActionRequest) (domain.Job, error)
	Resolve(ctx context.Context, s Scope, ref id.Ref, req api.ResolveJobRequest) (domain.Job, error)
	Logs(ctx context.Context, s Scope, ref id.Ref, q api.PageQuery) (api.List[domain.JobEvent], error)
	// Subscribe streams a job's events after afterSeq. The channel closes when the
	// job is terminal and drained, or when ctx ends.
	Subscribe(ctx context.Context, s Scope, ref id.Ref, afterSeq int64) (<-chan domain.JobEvent, error)
}

// Approvals is implemented by component engine. Approve and Reject apply
// domain.MayDecide (D7) and resume the waiting job.
type Approvals interface {
	List(ctx context.Context, s Scope, q api.ApprovalQuery) (api.List[domain.Approval], error)
	Get(ctx context.Context, s Scope, ref id.Ref) (domain.Approval, error)
	Approve(ctx context.Context, s Scope, ref id.Ref, req api.ApproveRequest) (domain.Approval, error)
	Reject(ctx context.Context, s Scope, ref id.Ref, req api.RejectRequest) (domain.Approval, error)
}

// Events is implemented by component engine.
type Events interface {
	List(ctx context.Context, s Scope, q api.EventQuery) (api.List[event.Envelope], error)
	Deliveries(ctx context.Context, s Scope, q api.DeliveryQuery) (api.List[event.Delivery], error)
	// Subscribe streams events after a tenant-local sequence, filtered by q.Type.
	Subscribe(ctx context.Context, s Scope, q api.EventQuery, afterSeq int64) (<-chan StreamedEvent, error)
}

// StreamedEvent is an event with its sequence, the SSE id.
type StreamedEvent struct {
	Seq   int64
	Event event.Envelope
}

// Introspection reports nodes and hooks. Implemented by component engine.
type Introspection interface {
	Nodes(ctx context.Context, s Scope) ([]node.Descriptor, error)
	Node(ctx context.Context, s Scope, nodeType string) (node.Descriptor, error)
	Hooks(ctx context.Context, s Scope, workflowID string) ([]api.HookInfo, error)
	TestHook(ctx context.Context, s Scope, hookID string, req api.TestHookRequest) (map[string]any, error)
}

// Assets is implemented by component core. Deleting and exporting start workflows,
// so those two API routes call Workflows.Run instead.
type Assets interface {
	List(ctx context.Context, s Scope, q api.AssetQuery) (api.List[domain.Asset], error)
	Get(ctx context.Context, s Scope, ref id.Ref) (domain.Asset, error)
	Update(ctx context.Context, s Scope, ref id.Ref, req api.UpdateAssetRequest) (domain.Asset, error)
	Lineage(ctx context.Context, s Scope, ref id.Ref) (api.LineageResponse, error)
	Versions(ctx context.Context, s Scope, ref id.Ref) ([]domain.AssetVersion, error)
	SetAIProcessing(ctx context.Context, s Scope, ref id.Ref, req api.SetAIProcessingRequest) (domain.Asset, error)
	Consent(ctx context.Context, s Scope, ref id.Ref) (api.ConsentResponse, error)
	Transcript(ctx context.Context, s Scope, ref id.Ref, version string) (domain.Transcript, error)
	ListSensitive(ctx context.Context, s Scope, ref id.Ref, version string) ([]domain.SensitiveSegment, error)
	AddSensitive(ctx context.Context, s Scope, ref id.Ref, req api.SensitiveSegmentRequest) error
	RemoveSensitive(ctx context.Context, s Scope, ref id.Ref, req api.SensitiveSegmentRequest) error
	Collections(ctx context.Context, s Scope) ([]domain.Collection, error)
	SetCollectionAIProcessing(ctx context.Context, s Scope, name string, req api.SetAIProcessingRequest) (domain.Collection, error)
}

// Search is implemented by component ai: lexical and semantic, combined (I43).
type Search interface {
	Search(ctx context.Context, s Scope, q api.SearchQuery) (api.SearchResponse, error)
}

// Publications is implemented by component publishing.
type Publications interface {
	Request(ctx context.Context, s Scope, req api.CreatePublicationRequest) (api.RunWorkflowResponse, error)
	List(ctx context.Context, s Scope, q api.PublicationQuery) (api.List[domain.Publication], error)
	Get(ctx context.Context, s Scope, ref id.Ref) (domain.Publication, error)
	// Withdraw runs unpublish-video; idempotent and immediate (D17).
	Withdraw(ctx context.Context, s Scope, req api.WithdrawRequest) (api.RunWorkflowResponse, error)
	WithdrawOne(ctx context.Context, s Scope, ref id.Ref) (api.RunWorkflowResponse, error)
	Sync(ctx context.Context, s Scope, ref id.Ref) (domain.Publication, error)
	Destinations(ctx context.Context, s Scope) ([]api.DestinationInfo, error)
}

// Uploads is implemented by component api, which owns the spool and the protocol
// handlers (I20). Workers read completed uploads from the spool volume.
type Uploads interface {
	Create(ctx context.Context, s Scope, req api.CreateUploadRequest) (domain.Upload, error)
	Get(ctx context.Context, s Scope, ref id.Ref) (domain.Upload, error)
	Complete(ctx context.Context, s Scope, ref id.Ref) (domain.Upload, error)
	Abort(ctx context.Context, s Scope, ref id.Ref) error
}

// Storages is implemented by component media and runs inside the daemon.
type Storages interface {
	List(ctx context.Context, s Scope) ([]api.StorageInfo, error)
	Test(ctx context.Context, s Scope, role domain.StorageRole) (api.StorageTestResponse, error)
	Verify(ctx context.Context, s Scope, role domain.StorageRole, req api.StorageVerifyRequest) (api.StorageVerifyResponse, error)
	ProxyLink(ctx context.Context, s Scope, ref id.Ref) (api.ProxyLinkResponse, error)
}

// ProxyReader is an optional capability of Storages, discovered by type assertion. It
// opens a file of the proxy role so a reviewer can get something playable from the
// API, whatever the driver (§3.2 step 6, I285). It reads role proxy only.
type ProxyReader interface {
	// OpenAssetProxy opens the proxy of an asset's current version.
	OpenAssetProxy(ctx context.Context, s Scope, ref id.Ref) (io.ReadCloser, connector.ObjectInfo, error)
	// OpenProxyURI opens a storage://proxy/... reference, such as an approval's
	// preview. Any other role is refused.
	OpenProxyURI(ctx context.Context, s Scope, uri string) (io.ReadCloser, connector.ObjectInfo, error)
}

// Secrets is implemented by component core. Metadata only: no value ever leaves.
type Secrets interface {
	List(ctx context.Context, s Scope) ([]domain.SecretMeta, error)
	Put(ctx context.Context, s Scope, req api.PutSecretRequest) (domain.SecretMeta, error)
	Delete(ctx context.Context, s Scope, name string) error
	Test(ctx context.Context, s Scope, name string) (api.SecretTestResponse, error)
	Rotate(ctx context.Context, s Scope, name string, req api.RotateSecretRequest) (domain.SecretMeta, error)
}

// Devices is implemented by component core (D12). Scope.Tenant may be empty.
type Devices interface {
	CreatePairingCode(ctx context.Context, s Scope, req api.CreatePairingCodeRequest) (api.PairingCodeResponse, error)
	Pair(ctx context.Context, req api.PairDeviceRequest) (api.DeviceCredentialResponse, error)
	List(ctx context.Context, s Scope) ([]domain.Device, error)
	Revoke(ctx context.Context, s Scope, ref id.Ref) (domain.Device, error)
	Rotate(ctx context.Context, s Scope, ref id.Ref) (api.DeviceCredentialResponse, error)
}

// Tenants is implemented by component core.
type Tenants interface {
	List(ctx context.Context, p domain.Principal) ([]api.TenantInfo, error)
	Create(ctx context.Context, p domain.Principal, req api.CreateTenantRequest) (domain.Tenant, error)
}

// Users is implemented by component core.
type Users interface {
	List(ctx context.Context, s Scope) ([]domain.Membership, error)
	Add(ctx context.Context, s Scope, req api.AddUserRequest) (domain.Membership, error)
	Grant(ctx context.Context, s Scope, userRef string, req api.GrantRequest) (domain.Membership, error)
}

// Budgets is implemented by component core (D19). Check is what the engine calls
// before dispatching a paid node.
type Budgets interface {
	Status(ctx context.Context, s Scope) (domain.BudgetStatus, error)
	Set(ctx context.Context, s Scope, req api.SetBudgetRequest) (domain.BudgetStatus, error)
	Override(ctx context.Context, s Scope, req api.BudgetOverrideRequest) (domain.BudgetStatus, error)
	// Check returns errs.CodeBudgetExceeded when new paid operations are blocked.
	Check(ctx context.Context, tenant domain.TenantID) error
	// Record adds usage and fires budget.threshold_reached and budget.exceeded.
	Record(ctx context.Context, tenant domain.TenantID, usage []domain.UsageRecord) error
}

// Audit is implemented by component core.
type Audit interface {
	List(ctx context.Context, s Scope, q api.AuditQuery) (api.List[domain.AuditRecord], error)
	Verify(ctx context.Context, s Scope) (domain.AuditVerification, error)
	Export(ctx context.Context, s Scope) (api.AuditExportResponse, error)
}

// Plugins is implemented by component extensibility.
type Plugins interface {
	List(ctx context.Context, s Scope) ([]domain.PluginRecord, error)
	Get(ctx context.Context, s Scope, pluginID string) (domain.PluginRecord, error)
	Install(ctx context.Context, s Scope, req api.InstallPluginRequest) (domain.PluginRecord, error)
}

// Observability is implemented by component extensibility.
type Observability interface {
	Logs(ctx context.Context, s Scope, q api.LogQuery) (api.List[connector.LogRecord], error)
	SubscribeLogs(ctx context.Context, s Scope, q api.LogQuery) (<-chan connector.LogRecord, error)
	Sinks(ctx context.Context, s Scope) ([]api.ObservabilitySink, error)
	TestSink(ctx context.Context, s Scope, sinkID string) error
	SetSinkEnabled(ctx context.Context, s Scope, sinkID string, enabled bool) error
	// MetricsHandler serves GET /metrics.
	MetricsHandler() http.Handler
}

// Connectors reports registered drivers. *connector.Registry satisfies it.
type Connectors interface {
	List() []connector.Info
}

// Doctor runs `vidyom doctor` checks. Implemented by component api, which asks the
// other services and the listeners.
type Doctor interface {
	Run(ctx context.Context, s Scope, check string) (api.DoctorResponse, error)
}
