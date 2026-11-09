// Package api is the HTTP API v1 contract shared by the server and the CLI: the
// route table and the request and response types. Paths are tenant-free; the
// tenant travels in the Vidyom-Tenant header (D32). Bodies are snake_case (I15).
//
// Every path in §12.1 is present. Routes beyond §12.1 back CLI commands of §11
// and the worker protocol (I4).
package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// Prefix is the API root (D54).
const Prefix = "/api/v1"

// Header names.
const (
	HeaderTenant         = "Vidyom-Tenant" // D32
	HeaderRequestID      = "Vidyom-Request-Id"
	HeaderIdempotencyKey = "Idempotency-Key"
	HeaderTraceParent    = "traceparent"
	HeaderUploadOffset   = "Upload-Offset"   // I20
	HeaderUploadChecksum = "Upload-Checksum" // "sha256 <base64>", I20
	HeaderLastEventID    = "Last-Event-ID"   // SSE resume, I3
)

// TenantScope says whether a route needs the Vidyom-Tenant header (D32). A
// required route without it is rejected with errs.CodeTenantRequired.
type TenantScope string

const (
	TenantRequired TenantScope = "required"
	TenantNone     TenantScope = "none"
	TenantOptional TenantScope = "optional" // the handler decides; see Route.Dynamic
)

// AuthMode says who may call a route.
type AuthMode string

const (
	AuthUser   AuthMode = "user"   // device credential or Unix peer identity
	AuthNone   AuthMode = "none"   // health and pairing exchange only
	AuthWorker AuthMode = "worker" // worker token (I4)
)

// Route is one API operation.
type Route struct {
	Name       string
	Method     string
	Path       string // relative to Prefix, in net/http pattern syntax
	Tenant     TenantScope
	Auth       AuthMode
	Permission domain.Permission // checked by the server before the handler
	// Dynamic means the service decides authorization beyond Permission, because
	// it depends on the resource: approval kind, resolve on publication nodes,
	// pairing scope.
	Dynamic bool
	Stream  bool // Server-Sent Events (I3)
}

// Pattern returns the net/http ServeMux pattern.
func (r Route) Pattern() string { return r.Method + " " + Prefix + r.Path }

// Mutating reports whether the route changes state; these take idempotency keys
// and write an audit record (§12.1).
func (r Route) Mutating() bool {
	return r.Method != http.MethodGet && r.Method != http.MethodHead
}

// URL expands path parameters, given as name, value pairs. Values must already be
// valid IDs; they are not escaped beyond what paths need.
func (r Route) URL(params ...string) string {
	if len(params)%2 != 0 {
		panic("api: URL takes name, value pairs")
	}
	path := r.Path
	for i := 0; i < len(params); i += 2 {
		path = strings.Replace(path, "{"+params[i]+"}", params[i+1], 1)
	}
	if strings.Contains(path, "{") {
		panic(fmt.Sprintf("api: missing path parameter in %s", path))
	}
	return Prefix + path
}

// Route names. Clients and handlers refer to routes by these constants.
const (
	RouteHealth  = "health"
	RouteVersion = "version"
	RouteMetrics = "metrics"
	RouteDoctor  = "doctor"

	RouteWorkflowsList     = "workflows.list"
	RouteWorkflowsGet      = "workflows.get"
	RouteWorkflowsValidate = "workflows.validate"
	RouteWorkflowsRun      = "workflows.run"
	RouteWorkflowsRunBody  = "workflows.run_inline"

	RouteJobsList    = "jobs.list"
	RouteJobsGet     = "jobs.get"
	RouteJobsEvents  = "jobs.events"
	RouteJobsLogs    = "jobs.logs"
	RouteJobsCancel  = "jobs.cancel"
	RouteJobsRetry   = "jobs.retry"
	RouteJobsPause   = "jobs.pause"
	RouteJobsResume  = "jobs.resume"
	RouteJobsResolve = "jobs.resolve"

	RouteUploadsCreate   = "uploads.create"
	RouteUploadsGet      = "uploads.get"
	RouteUploadsChunk    = "uploads.chunk"
	RouteUploadsComplete = "uploads.complete"
	RouteUploadsAbort    = "uploads.abort"

	RouteAssetsList         = "assets.list"
	RouteAssetsGet          = "assets.get"
	RouteAssetsUpdate       = "assets.update"
	RouteAssetsDelete       = "assets.delete"
	RouteAssetsLineage      = "assets.lineage"
	RouteAssetsVersions     = "assets.versions"
	RouteAssetsAIProcessing = "assets.ai_processing"
	RouteAssetsConsent      = "assets.consent"
	RouteAssetsTranscript   = "assets.transcript"
	RouteAssetsProxyLink    = "assets.proxy_link"
	RouteAssetsExport       = "assets.export"
	RouteSensitiveList      = "assets.sensitive.list"
	RouteSensitiveAdd       = "assets.sensitive.add"
	RouteSensitiveRemove    = "assets.sensitive.remove"

	RouteCollectionsList         = "collections.list"
	RouteCollectionsAIProcessing = "collections.ai_processing"

	RouteClipsCreate = "clips.create"
	RouteSearch      = "search"

	RouteApprovalsList    = "approvals.list"
	RouteApprovalsGet     = "approvals.get"
	RouteApprovalsApprove = "approvals.approve"
	RouteApprovalsReject  = "approvals.reject"

	RoutePublicationsCreate   = "publications.create"
	RoutePublicationsList     = "publications.list"
	RoutePublicationsGet      = "publications.get"
	RoutePublicationsWithdraw = "publications.withdraw"
	RoutePublicationsUnpub    = "publications.unpublish"
	RoutePublicationsSync     = "publications.sync"
	RouteDestinationsList     = "publishing.destinations"

	RouteNodesList    = "nodes.list"
	RouteNodesGet     = "nodes.get"
	RouteNodesRun     = "nodes.run"
	RouteConnectors   = "connectors.list"
	RoutePluginsList  = "plugins.list"
	RoutePluginsGet   = "plugins.get"
	RoutePluginsAdd   = "plugins.install"
	RouteHooksList    = "hooks.list"
	RouteHooksTest    = "hooks.test"
	RouteEventsList   = "events.list"
	RouteEventsDeliv  = "events.deliveries"
	RouteEventsStream = "events.stream"

	RouteStoragesList    = "storages.list"
	RouteStoragesTest    = "storages.test"
	RouteStoragesVerify  = "storages.verify"
	RouteStoragesCopy    = "storages.copy"
	RouteStoragesRestore = "storages.restore"

	RouteSecretsList   = "secrets.list"
	RouteSecretsPut    = "secrets.put"
	RouteSecretsDelete = "secrets.delete"
	RouteSecretsTest   = "secrets.test"
	RouteSecretsRotate = "secrets.rotate"

	RoutePairingCodes  = "pairing_codes.create"
	RouteDevicesPair   = "devices.pair"
	RouteDevicesList   = "devices.list"
	RouteDevicesRevoke = "devices.revoke"
	RouteDevicesRotate = "devices.rotate"
	RouteClientsList   = "clients.list"
	RouteClientsRevoke = "clients.revoke"

	RouteTenantsList = "tenants.list"
	RouteTenantsAdd  = "tenants.create"
	RouteUsersList   = "users.list"
	RouteUsersAdd    = "users.add"
	RouteUsersGrant  = "users.grant"
	RouteBudgetGet   = "budget.get"
	RouteBudgetPut   = "budget.put"
	RouteBudgetOver  = "budget.override"
	RouteAuditList   = "audit.list"
	RouteAuditVerify = "audit.verify"
	RouteAuditExport = "audit.export"
	RouteLogsList    = "logs.list"
	RouteLogsStream  = "logs.stream"
	RouteObsList     = "observability.list"
	RouteObsTest     = "observability.test"
	RouteObsEnable   = "observability.enable"
	RouteObsDisable  = "observability.disable"
	RouteObsStatus   = "observability.status"

	RouteWorkerRegister      = "worker.register"
	RouteWorkerHeartbeat     = "worker.heartbeat"
	RouteWorkerLease         = "worker.tasks.lease"
	RouteWorkerTaskHeartbeat = "worker.tasks.heartbeat"
	RouteWorkerTaskLogs      = "worker.tasks.logs"
	RouteWorkerComplete      = "worker.tasks.complete"
	RouteWorkerFail          = "worker.tasks.fail"
	RouteWorkerConnection    = "worker.tasks.connection"
	RouteWorkerCacheKey      = "worker.tasks.cache_key"
)

func route(name, method, path string, tenant TenantScope, perm domain.Permission) Route {
	return Route{Name: name, Method: method, Path: path, Tenant: tenant, Auth: AuthUser, Permission: perm}
}

func dynamic(r Route) Route { r.Dynamic = true; return r }
func stream(r Route) Route  { r.Stream = true; return r }
func worker(name, path string) Route {
	return Route{Name: name, Method: http.MethodPost, Path: path, Tenant: TenantNone, Auth: AuthWorker}
}

const (
	mGet    = http.MethodGet
	mPost   = http.MethodPost
	mPut    = http.MethodPut
	mPatch  = http.MethodPatch
	mDelete = http.MethodDelete
)

// Routes is the whole API. The server registers exactly these; the CLI builds
// URLs only from these.
var Routes = []Route{
	{Name: RouteHealth, Method: mGet, Path: "/health", Tenant: TenantNone, Auth: AuthNone},
	{Name: RouteVersion, Method: mGet, Path: "/version", Tenant: TenantNone, Auth: AuthNone},
	dynamic(route(RouteMetrics, mGet, "/metrics", TenantOptional, domain.PermMetricsRead)),
	dynamic(route(RouteDoctor, mGet, "/doctor", TenantOptional, "")),

	route(RouteWorkflowsList, mGet, "/workflows", TenantRequired, domain.PermWorkflowRead),
	route(RouteWorkflowsGet, mGet, "/workflows/{id}", TenantRequired, domain.PermWorkflowRead),
	route(RouteWorkflowsValidate, mPost, "/workflows/validate", TenantRequired, domain.PermWorkflowRead),
	route(RouteWorkflowsRun, mPost, "/workflows/{id}/runs", TenantRequired, domain.PermWorkflowRun),
	route(RouteWorkflowsRunBody, mPost, "/workflows/runs", TenantRequired, domain.PermWorkflowRun),

	route(RouteJobsList, mGet, "/jobs", TenantRequired, domain.PermJobRead),
	route(RouteJobsGet, mGet, "/jobs/{id}", TenantRequired, domain.PermJobRead),
	stream(route(RouteJobsEvents, mGet, "/jobs/{id}/events", TenantRequired, domain.PermJobRead)),
	route(RouteJobsLogs, mGet, "/jobs/{id}/logs", TenantRequired, domain.PermJobRead),
	route(RouteJobsCancel, mPost, "/jobs/{id}/cancel", TenantRequired, domain.PermJobControl),
	route(RouteJobsRetry, mPost, "/jobs/{id}/retry", TenantRequired, domain.PermJobControl),
	route(RouteJobsPause, mPost, "/jobs/{id}/pause", TenantRequired, domain.PermJobControl),
	route(RouteJobsResume, mPost, "/jobs/{id}/resume", TenantRequired, domain.PermJobControl),
	dynamic(route(RouteJobsResolve, mPost, "/jobs/{id}/resolve", TenantRequired, domain.PermJobResolve)),

	route(RouteUploadsCreate, mPost, "/assets/uploads", TenantRequired, domain.PermUploadCreate),
	route(RouteUploadsGet, mGet, "/uploads/{id}", TenantRequired, domain.PermUploadCreate),
	route(RouteUploadsChunk, mPatch, "/uploads/{id}", TenantRequired, domain.PermUploadCreate),
	route(RouteUploadsComplete, mPost, "/uploads/{id}/complete", TenantRequired, domain.PermUploadCreate),
	route(RouteUploadsAbort, mDelete, "/uploads/{id}", TenantRequired, domain.PermUploadCreate),

	route(RouteAssetsList, mGet, "/assets", TenantRequired, domain.PermAssetRead),
	route(RouteAssetsGet, mGet, "/assets/{id}", TenantRequired, domain.PermAssetRead),
	route(RouteAssetsUpdate, mPatch, "/assets/{id}", TenantRequired, domain.PermAssetWrite),
	route(RouteAssetsDelete, mDelete, "/assets/{id}", TenantRequired, domain.PermAssetDelete),
	route(RouteAssetsLineage, mGet, "/assets/{id}/lineage", TenantRequired, domain.PermAssetRead),
	route(RouteAssetsVersions, mGet, "/assets/{id}/versions", TenantRequired, domain.PermAssetRead),
	route(RouteAssetsAIProcessing, mPut, "/assets/{id}/ai-processing", TenantRequired, domain.PermAssetAIConsent),
	route(RouteAssetsConsent, mGet, "/assets/{id}/consent", TenantRequired, domain.PermAssetRead),
	route(RouteAssetsTranscript, mGet, "/assets/{id}/transcript", TenantRequired, domain.PermAssetRead),
	route(RouteAssetsProxyLink, mGet, "/assets/{id}/proxy-link", TenantRequired, domain.PermAssetRead),
	route(RouteAssetsExport, mPost, "/assets/{id}/export", TenantRequired, domain.PermAssetRead),
	route(RouteSensitiveList, mGet, "/assets/{id}/sensitive-segments", TenantRequired, domain.PermAssetRead),
	route(RouteSensitiveAdd, mPost, "/assets/{id}/sensitive-segments", TenantRequired, domain.PermAssetSensitive),
	route(RouteSensitiveRemove, mDelete, "/assets/{id}/sensitive-segments", TenantRequired, domain.PermAssetSensitive),

	route(RouteCollectionsList, mGet, "/collections", TenantRequired, domain.PermAssetRead),
	route(RouteCollectionsAIProcessing, mPut, "/collections/{name}/ai-processing", TenantRequired, domain.PermAssetAIConsent),

	route(RouteClipsCreate, mPost, "/clips", TenantRequired, domain.PermWorkflowRun),
	route(RouteSearch, mGet, "/search", TenantRequired, domain.PermSearch),

	route(RouteApprovalsList, mGet, "/approvals", TenantRequired, domain.PermApprovalRead),
	route(RouteApprovalsGet, mGet, "/approvals/{id}", TenantRequired, domain.PermApprovalRead),
	dynamic(route(RouteApprovalsApprove, mPost, "/approvals/{id}/approve", TenantRequired, "")),
	dynamic(route(RouteApprovalsReject, mPost, "/approvals/{id}/reject", TenantRequired, "")),

	route(RoutePublicationsCreate, mPost, "/publications", TenantRequired, domain.PermPublicationCreate),
	route(RoutePublicationsList, mGet, "/publications", TenantRequired, domain.PermPublicationRead),
	route(RoutePublicationsGet, mGet, "/publications/{id}", TenantRequired, domain.PermPublicationRead),
	route(RoutePublicationsWithdraw, mDelete, "/publications/{id}", TenantRequired, domain.PermPublicationWithdraw),
	route(RoutePublicationsUnpub, mPost, "/publications/withdraw", TenantRequired, domain.PermPublicationWithdraw),
	route(RoutePublicationsSync, mPost, "/publications/{id}/sync", TenantRequired, domain.PermPublicationRead),
	route(RouteDestinationsList, mGet, "/publishing/destinations", TenantRequired, domain.PermPublicationRead),

	route(RouteNodesList, mGet, "/nodes", TenantRequired, domain.PermNodeRead),
	route(RouteNodesGet, mGet, "/nodes/{type}", TenantRequired, domain.PermNodeRead),
	route(RouteNodesRun, mPost, "/nodes/{type}/runs", TenantRequired, domain.PermNodeRun),
	route(RouteConnectors, mGet, "/connectors", TenantRequired, domain.PermConnectorRead),
	route(RoutePluginsList, mGet, "/plugins", TenantRequired, domain.PermPluginRead),
	route(RoutePluginsGet, mGet, "/plugins/{id}", TenantRequired, domain.PermPluginRead),
	route(RoutePluginsAdd, mPost, "/plugins", TenantRequired, domain.PermPluginInstall),
	route(RouteHooksList, mGet, "/hooks", TenantRequired, domain.PermHookRead),
	route(RouteHooksTest, mPost, "/hooks/{id}/test", TenantRequired, domain.PermHookTest),
	route(RouteEventsList, mGet, "/events", TenantRequired, domain.PermEventRead),
	route(RouteEventsDeliv, mGet, "/events/deliveries", TenantRequired, domain.PermEventRead),
	stream(route(RouteEventsStream, mGet, "/events/stream", TenantRequired, domain.PermEventRead)),

	route(RouteStoragesList, mGet, "/storages", TenantRequired, domain.PermStorageRead),
	route(RouteStoragesTest, mPost, "/storages/{role}/test", TenantRequired, domain.PermStorageTest),
	route(RouteStoragesVerify, mPost, "/storages/{role}/verify", TenantRequired, domain.PermStorageTest),
	route(RouteStoragesCopy, mPost, "/storages/copy", TenantRequired, domain.PermStorageCopy),
	route(RouteStoragesRestore, mPost, "/storages/restore", TenantRequired, domain.PermStorageArchiveRestore),

	route(RouteSecretsList, mGet, "/secrets", TenantRequired, domain.PermSecretRead),
	route(RouteSecretsPut, mPost, "/secrets", TenantRequired, domain.PermSecretWrite),
	route(RouteSecretsDelete, mDelete, "/secrets/{name}", TenantRequired, domain.PermSecretWrite),
	route(RouteSecretsTest, mPost, "/secrets/{name}/test", TenantRequired, domain.PermSecretRead),
	route(RouteSecretsRotate, mPost, "/secrets/{name}/rotate", TenantRequired, domain.PermSecretWrite),

	dynamic(route(RoutePairingCodes, mPost, "/pairing-codes", TenantOptional, domain.PermDeviceManage)),
	{Name: RouteDevicesPair, Method: mPost, Path: "/devices/pair", Tenant: TenantNone, Auth: AuthNone},
	dynamic(route(RouteDevicesList, mGet, "/devices", TenantOptional, "")),
	dynamic(route(RouteDevicesRevoke, mPost, "/devices/{id}/revoke", TenantOptional, domain.PermDeviceManage)),
	dynamic(route(RouteDevicesRotate, mPost, "/devices/{id}/rotate", TenantOptional, "")),
	dynamic(route(RouteClientsList, mGet, "/clients", TenantOptional, "")),
	dynamic(route(RouteClientsRevoke, mPost, "/clients/{id}/revoke", TenantOptional, domain.PermDeviceManage)),

	dynamic(route(RouteTenantsList, mGet, "/tenants", TenantNone, "")),
	route(RouteTenantsAdd, mPost, "/tenants", TenantNone, domain.PermTenantCreate),
	route(RouteUsersList, mGet, "/users", TenantRequired, domain.PermUserRead),
	route(RouteUsersAdd, mPost, "/users", TenantRequired, domain.PermUserManage),
	route(RouteUsersGrant, mPost, "/users/{id}/grants", TenantRequired, domain.PermUserManage),
	route(RouteBudgetGet, mGet, "/budget", TenantRequired, domain.PermBudgetRead),
	route(RouteBudgetPut, mPut, "/budget", TenantRequired, domain.PermBudgetManage),
	route(RouteBudgetOver, mPost, "/budget/override", TenantRequired, domain.PermBudgetOverride),
	route(RouteAuditList, mGet, "/audit", TenantRequired, domain.PermAuditRead),
	route(RouteAuditVerify, mGet, "/audit/verify", TenantRequired, domain.PermAuditRead),
	route(RouteAuditExport, mPost, "/audit/export", TenantRequired, domain.PermAuditExport),
	route(RouteLogsList, mGet, "/logs", TenantRequired, domain.PermObservabilityRead),
	stream(route(RouteLogsStream, mGet, "/logs/stream", TenantRequired, domain.PermObservabilityRead)),
	route(RouteObsList, mGet, "/observability", TenantRequired, domain.PermObservabilityRead),
	route(RouteObsStatus, mGet, "/observability/status", TenantRequired, domain.PermObservabilityRead),
	route(RouteObsTest, mPost, "/observability/{id}/test", TenantRequired, domain.PermObservabilityManage),
	route(RouteObsEnable, mPost, "/observability/{id}/enable", TenantRequired, domain.PermObservabilityManage),
	route(RouteObsDisable, mPost, "/observability/{id}/disable", TenantRequired, domain.PermObservabilityManage),

	worker(RouteWorkerRegister, "/worker/register"),
	worker(RouteWorkerHeartbeat, "/worker/heartbeat"),
	worker(RouteWorkerLease, "/worker/tasks/{id}/lease"),
	worker(RouteWorkerTaskHeartbeat, "/worker/tasks/{id}/heartbeat"),
	worker(RouteWorkerTaskLogs, "/worker/tasks/{id}/logs"),
	worker(RouteWorkerComplete, "/worker/tasks/{id}/complete"),
	worker(RouteWorkerFail, "/worker/tasks/{id}/fail"),
	worker(RouteWorkerConnection, "/worker/tasks/{id}/connection"),
	worker(RouteWorkerCacheKey, "/worker/tasks/{id}/cache-key"),
}

var routesByName = func() map[string]Route {
	m := make(map[string]Route, len(Routes))
	for _, r := range Routes {
		if _, dup := m[r.Name]; dup {
			panic("api: duplicate route name " + r.Name)
		}
		m[r.Name] = r
	}
	return m
}()

// ByName returns a route by its constant name. An unknown name is a programming
// error and panics.
func ByName(name string) Route {
	r, ok := routesByName[name]
	if !ok {
		panic("api: unknown route " + name)
	}
	return r
}

// RouteNames lists all names, sorted.
func RouteNames() []string {
	names := make([]string, 0, len(routesByName))
	for n := range routesByName {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
