package apiserver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/rafaelp/vidyom/internal/api/uploads"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
)

// handlers maps every route name to its handler. A group whose service is nil
// answers 501 naming it, so partial wiring is visible rather than a crash.
func (s *server) handlers() map[string]handlerFunc {
	out := map[string]handlerFunc{}
	add := func(wired bool, what string, group map[string]handlerFunc) {
		for name, h := range group {
			if !wired {
				h = unwired(what)
			}
			out[name] = h
		}
	}

	add(true, "", map[string]handlerFunc{
		api.RouteHealth:  s.handleHealth,
		api.RouteVersion: s.handleVersion,
	})
	add(s.d.Observability != nil, "the observability service", s.observabilityHandlers())
	add(s.d.Doctor != nil, "the doctor service", map[string]handlerFunc{api.RouteDoctor: s.handleDoctor})
	add(s.d.Workflows != nil, "the workflow service", s.workflowHandlers())
	add(s.d.Jobs != nil, "the job service", s.jobHandlers())
	add(s.d.Uploads != nil, "the upload service", s.uploadHandlers())
	add(s.d.Assets != nil, "the asset service", s.assetHandlers())
	add(s.d.Workflows != nil && s.d.Assets != nil, "the workflow and asset services", map[string]handlerFunc{
		api.RouteStoragesCopy:    s.handleStorageCopy,
		api.RouteStoragesRestore: s.handleStorageRestore,
	})
	add(s.d.Storages != nil, "the storage service", s.storageHandlers())
	add(s.d.Search != nil, "the search service", map[string]handlerFunc{api.RouteSearch: s.handleSearch})
	add(s.d.Approvals != nil, "the approval service", s.approvalHandlers())
	add(s.d.Publications != nil, "the publication service", s.publicationHandlers())
	add(s.d.Introspection != nil, "the introspection service", s.introspectionHandlers())
	add(s.d.Connectors != nil, "the connector registry", map[string]handlerFunc{
		api.RouteConnectors: func(c *call) error {
			list, err := listOf[connector.Info](s.d.Connectors.List(), nil)
			return reply(c, http.StatusOK, list, err)
		},
	})
	add(s.d.Plugins != nil, "the plugin service", s.pluginHandlers())
	add(s.d.Events != nil, "the event service", s.eventHandlers())
	add(s.d.Secrets != nil, "the secret service", s.secretHandlers())
	add(s.d.Devices != nil, "the device service", s.deviceHandlers())
	add(s.d.Tenants != nil, "the tenant service", s.tenantHandlers())
	add(s.d.Users != nil, "the user service", s.userHandlers())
	add(s.d.Budgets != nil, "the budget service", s.budgetHandlers())
	add(s.d.Audit != nil, "the audit service", s.auditHandlers())
	add(s.d.Coordinator != nil, "the worker coordinator", s.workerHandlers())
	return out
}

// jobStatus is 202 when a job started and 200 for a dry run, which starts nothing.
func jobStatus(resp api.RunWorkflowResponse) int {
	if resp.JobID == "" && resp.JobULID == "" {
		return http.StatusOK
	}
	return http.StatusAccepted
}

func replyRun(c *call, resp api.RunWorkflowResponse, err error) error {
	return reply(c, jobStatus(resp), resp, err)
}

// ---- Workflows ----

func (s *server) workflowHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteWorkflowsList: func(c *call) error {
			items, err := s.d.Workflows.List(c.ctx, c.scope)
			list, err := listOf(items, err)
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteWorkflowsGet: func(c *call) error {
			out, err := s.d.Workflows.Get(c.ctx, c.scope, c.param("id"), c.query("version"))
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteWorkflowsValidate: func(c *call) error {
			var req api.ValidateWorkflowRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			if len(req.Definition) == 0 {
				return errs.New(errs.CodeInvalidArgument, "definition is required")
			}
			out, err := s.d.Workflows.Validate(c.ctx, c.scope, req)
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteWorkflowsRun:     s.handleWorkflowRun,
		api.RouteWorkflowsRunBody: s.handleWorkflowRunInline,
		api.RouteNodesRun:         s.handleNodeRun,
		api.RouteClipsCreate: func(c *call) error {
			var req api.CreateClipRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Workflows.CreateClip(c.ctx, c.scope, req)
			return replyRun(c, out, err)
		},
	}
}

func (s *server) handleWorkflowRun(c *call) error {
	var req api.RunWorkflowRequest
	if err := c.decodeOptional(&req); err != nil {
		return err
	}
	if len(req.Definition) != 0 {
		return errs.New(errs.CodeInvalidArgument, "this route runs a registered workflow; send inline definitions to /workflows/runs")
	}
	workflow := c.param("id")
	detail, err := s.d.Workflows.Get(c.ctx, c.scope, workflow, req.WorkflowVersion)
	if err != nil {
		return err
	}
	if req.Inputs, err = s.checkInputs(c, detail.Inputs, req.Inputs); err != nil {
		return err
	}
	req.IdempotencyKey = c.scope.IdempotencyKey
	out, err := s.d.Workflows.Run(c.ctx, c.scope, workflow, req)
	return replyRun(c, out, err)
}

func (s *server) handleWorkflowRunInline(c *call) error {
	var req api.RunWorkflowRequest
	if err := c.decode(&req); err != nil {
		return err
	}
	if len(req.Definition) == 0 {
		return errs.New(errs.CodeInvalidArgument, "definition is required")
	}
	def, err := wfdef.Parse(req.Definition)
	if err != nil {
		return errs.Wrap(errs.CodeValidationFailed, err, "the definition is not a workflow")
	}
	if err := s.checkInlineDefinition(c, def); err != nil {
		return err
	}
	if req.Inputs, err = s.checkInputs(c, def.Inputs, req.Inputs); err != nil {
		return err
	}
	req.IdempotencyKey = c.scope.IdempotencyKey
	out, err := s.d.Workflows.Run(c.ctx, c.scope, "", req)
	return replyRun(c, out, err)
}

func (s *server) handleNodeRun(c *call) error {
	var req api.RunNodeRequest
	if err := c.decodeOptional(&req); err != nil {
		return err
	}
	var err error
	if req.Parameters, err = s.checkNodeParameters(c, req.Parameters); err != nil {
		return err
	}
	out, err := s.d.Workflows.RunNode(c.ctx, c.scope, c.param("type"), req)
	return replyRun(c, out, err)
}

// ---- Jobs ----

func (s *server) jobHandlers() map[string]handlerFunc {
	action := func(run func(c *call, req api.JobActionRequest) (domain.Job, error)) handlerFunc {
		return func(c *call) error {
			var req api.JobActionRequest
			if err := c.decodeOptional(&req); err != nil {
				return err
			}
			out, err := run(c, req)
			return reply(c, http.StatusOK, out, err)
		}
	}
	return map[string]handlerFunc{
		api.RouteJobsList: func(c *call) error {
			paging, err := c.pageQuery()
			if err != nil {
				return err
			}
			q := api.JobQuery{
				PageQuery: paging,
				Status:    domain.JobStatus(c.query("status")),
				Workflow:  c.query("workflow"),
				AssetID:   c.query("asset_id"),
			}
			list, err := normalized(s.d.Jobs.List(c.ctx, c.scope, q))
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteJobsGet: func(c *call) error {
			out, err := s.d.Jobs.Get(c.ctx, c.scope, c.ref())
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteJobsEvents: s.handleJobEvents,
		api.RouteJobsLogs: func(c *call) error {
			q, err := c.pageQuery()
			if err != nil {
				return err
			}
			list, err := normalized(s.d.Jobs.Logs(c.ctx, c.scope, c.ref(), q))
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteJobsCancel: action(func(c *call, r api.JobActionRequest) (domain.Job, error) {
			return s.d.Jobs.Cancel(c.ctx, c.scope, c.ref(), r)
		}),
		api.RouteJobsRetry: action(func(c *call, r api.JobActionRequest) (domain.Job, error) {
			return s.d.Jobs.Retry(c.ctx, c.scope, c.ref(), r)
		}),
		api.RouteJobsPause: action(func(c *call, r api.JobActionRequest) (domain.Job, error) {
			return s.d.Jobs.Pause(c.ctx, c.scope, c.ref(), r)
		}),
		api.RouteJobsResume: action(func(c *call, r api.JobActionRequest) (domain.Job, error) {
			return s.d.Jobs.Resume(c.ctx, c.scope, c.ref(), r)
		}),
		api.RouteJobsResolve: func(c *call) error {
			var req api.ResolveJobRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Jobs.Resolve(c.ctx, c.scope, c.ref(), req)
			return reply(c, http.StatusOK, out, err)
		},
	}
}

func (s *server) handleJobEvents(c *call) error {
	after, err := c.lastEventID()
	if err != nil {
		return err
	}
	ctx, cancel := subscription(c)
	defer cancel()
	ch, err := s.d.Jobs.Subscribe(ctx, c.scope, c.ref(), after)
	if err != nil {
		return err
	}
	return serveSSE(s, c, ch, func(e domain.JobEvent) frame {
		return frame{id: seqID(e.Seq), event: api.SSEJobEvent, data: e}
	})
}

// ---- Uploads ----

func (s *server) uploadHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteUploadsCreate: func(c *call) error {
			var req api.CreateUploadRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Uploads.Create(c.ctx, c.scope, req)
			if err == nil {
				c.w.Header().Set(api.HeaderUploadOffset, strconv.FormatInt(out.ReceivedBytes, 10))
			}
			return reply(c, http.StatusCreated, out, err)
		},
		api.RouteUploadsGet: func(c *call) error {
			out, err := s.d.Uploads.Get(c.ctx, c.scope, c.ref())
			if err == nil {
				c.w.Header().Set(api.HeaderUploadOffset, strconv.FormatInt(out.ReceivedBytes, 10))
			}
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteUploadsChunk: s.handleUploadChunk,
		api.RouteUploadsComplete: func(c *call) error {
			out, err := s.d.Uploads.Complete(c.ctx, c.scope, c.ref())
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteUploadsAbort: func(c *call) error {
			return reply(c, http.StatusOK, empty, s.d.Uploads.Abort(c.ctx, c.scope, c.ref()))
		},
	}
}

// parseChecksum reads "sha256 <base64>" (I20).
func parseChecksum(raw string) ([]byte, error) {
	if raw == "" {
		return nil, nil
	}
	algorithm, encoded, ok := strings.Cut(strings.TrimSpace(raw), " ")
	if !ok || !strings.EqualFold(algorithm, "sha256") {
		return nil, errs.Newf(errs.CodeInvalidArgument, "%s must be \"sha256 <base64>\"", api.HeaderUploadChecksum)
	}
	sum, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(sum) != 32 {
		return nil, errs.Newf(errs.CodeInvalidArgument, "%s must hold a base64 SHA-256", api.HeaderUploadChecksum)
	}
	return sum, nil
}

func (s *server) handleUploadChunk(c *call) error {
	raw := c.r.Header.Get(api.HeaderUploadOffset)
	offset, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || offset < 0 {
		return errs.Newf(errs.CodeInvalidArgument, "%s must be a non-negative integer", api.HeaderUploadOffset)
	}
	sum, err := parseChecksum(c.r.Header.Get(api.HeaderUploadChecksum))
	if err != nil {
		return err
	}
	out, err := s.d.Uploads.AppendChunk(c.ctx, c.scope, c.ref(), uploads.Chunk{
		Offset: offset, SHA256: sum, Body: s.chunkReader(c),
	})
	if err != nil {
		return err
	}
	c.w.Header().Set(api.HeaderUploadOffset, strconv.FormatInt(out.ReceivedBytes, 10))
	return reply(c, http.StatusOK, out, nil)
}

// ---- Assets ----

func (s *server) assetHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteAssetsList: func(c *call) error {
			paging, err := c.pageQuery()
			if err != nil {
				return err
			}
			q := api.AssetQuery{
				PageQuery:  paging,
				Kind:       domain.AssetKind(c.query("kind")),
				Status:     domain.AssetStatus(c.query("status")),
				SourceID:   c.query("source_id"),
				Collection: c.query("collection"),
			}
			list, err := normalized(s.d.Assets.List(c.ctx, c.scope, q))
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteAssetsGet: func(c *call) error {
			out, err := s.d.Assets.Get(c.ctx, c.scope, c.ref())
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteAssetsUpdate: func(c *call) error {
			var req api.UpdateAssetRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Assets.Update(c.ctx, c.scope, c.ref(), req)
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteAssetsDelete: s.handleAssetDelete,
		api.RouteAssetsLineage: func(c *call) error {
			out, err := s.d.Assets.Lineage(c.ctx, c.scope, c.ref())
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteAssetsVersions: func(c *call) error {
			items, err := s.d.Assets.Versions(c.ctx, c.scope, c.ref())
			list, err := listOf(items, err)
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteAssetsAIProcessing: func(c *call) error {
			var req api.SetAIProcessingRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Assets.SetAIProcessing(c.ctx, c.scope, c.ref(), req)
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteAssetsConsent: func(c *call) error {
			out, err := s.d.Assets.Consent(c.ctx, c.scope, c.ref())
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteAssetsTranscript: func(c *call) error {
			out, err := s.d.Assets.Transcript(c.ctx, c.scope, c.ref(), c.query("version"))
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteAssetsProxyLink: func(c *call) error {
			if s.d.Storages == nil {
				return unwired("the storage service")(c)
			}
			out, err := s.d.Storages.ProxyLink(c.ctx, c.scope, c.ref())
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteAssetsExport: s.handleAssetExport,
		api.RouteSensitiveList: func(c *call) error {
			items, err := s.d.Assets.ListSensitive(c.ctx, c.scope, c.ref(), c.query("version"))
			list, err := listOf(items, err)
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteSensitiveAdd: func(c *call) error {
			var req api.SensitiveSegmentRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			return reply(c, http.StatusCreated, empty, s.d.Assets.AddSensitive(c.ctx, c.scope, c.ref(), req))
		},
		api.RouteSensitiveRemove: func(c *call) error {
			var req api.SensitiveSegmentRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			return reply(c, http.StatusOK, empty, s.d.Assets.RemoveSensitive(c.ctx, c.scope, c.ref(), req))
		},
		api.RouteCollectionsList: func(c *call) error {
			items, err := s.d.Assets.Collections(c.ctx, c.scope)
			list, err := listOf(items, err)
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteCollectionsAIProcessing: func(c *call) error {
			var req api.SetAIProcessingRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Assets.SetCollectionAIProcessing(c.ctx, c.scope, c.param("name"), req)
			return reply(c, http.StatusOK, out, err)
		},
	}
}

// handleAssetDelete runs delete-asset (R16, D17, I132). The workflow refuses an
// asset with active publications and reports the early-deletion estimate.
func (s *server) handleAssetDelete(c *call) error {
	if s.d.Workflows == nil {
		return unwired("the workflow service")(c)
	}
	inputs, err := json.Marshal(map[string]string{"asset_id": string(c.ref())})
	if err != nil {
		return err
	}
	out, err := s.d.Workflows.Run(c.ctx, c.scope, "delete-asset", api.RunWorkflowRequest{
		Inputs: inputs, IdempotencyKey: c.scope.IdempotencyKey,
	})
	return replyRun(c, out, err)
}

// handleAssetExport builds the metadata package of §12.5 as an ad-hoc job (I132).
func (s *server) handleAssetExport(c *call) error {
	if s.d.Workflows == nil {
		return unwired("the workflow service")(c)
	}
	var ignored struct{}
	if err := c.decodeOptional(&ignored); err != nil {
		return err
	}
	params, err := json.Marshal(map[string]string{"assetId": string(c.ref())})
	if err != nil {
		return err
	}
	out, err := s.d.Workflows.RunNode(c.ctx, c.scope, "catalog.export_package", api.RunNodeRequest{
		Parameters: params, AssetID: string(c.ref()),
	})
	return replyRun(c, out, err)
}

// ---- Storage ----

func (s *server) storageHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteStoragesList: func(c *call) error {
			items, err := s.d.Storages.List(c.ctx, c.scope)
			list, err := listOf(items, err)
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteStoragesTest: func(c *call) error {
			out, err := s.d.Storages.Test(c.ctx, c.scope, domain.StorageRole(c.param("role")))
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteStoragesVerify: func(c *call) error {
			var req api.StorageVerifyRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Storages.Verify(c.ctx, c.scope, domain.StorageRole(c.param("role")), req)
			return reply(c, http.StatusOK, out, err)
		},
	}
}

func assetRefFrom(raw string) (id.Ref, error) {
	ref := id.Ref(raw)
	if !validRefFor(raw, refPrefixes["assets"]) {
		return "", errs.New(errs.CodeInvalidArgument, "asset_id must name an asset")
	}
	return ref, nil
}

func (s *server) filesInRole(c *call, ref id.Ref, role domain.StorageRole) (domain.Asset, []domain.MediaFile, error) {
	asset, err := s.d.Assets.Get(c.ctx, c.scope, ref)
	if err != nil {
		return domain.Asset{}, nil, err
	}
	versions, err := s.d.Assets.Versions(c.ctx, c.scope, ref)
	if err != nil {
		return domain.Asset{}, nil, err
	}
	var files []domain.MediaFile
	for _, v := range versions {
		if v.Version != asset.CurrentVersion {
			continue
		}
		for _, f := range v.Files {
			if f.File.Storage == role {
				files = append(files, f.File)
			}
		}
	}
	return asset, files, nil
}

// handleStorageCopy runs one storage.copy node per file of the asset's current
// version in the source role, as a tracked job (I132). Reading archive is only for
// restore-archived, which needs its own permission (§3.3).
func (s *server) handleStorageCopy(c *call) error {
	var req api.StorageCopyRequest
	if err := c.decode(&req); err != nil {
		return err
	}
	if !req.From.Valid() || !req.To.Valid() {
		return errs.New(errs.CodeInvalidArgument, "from and to must be storage roles")
	}
	if req.From == req.To {
		return errs.New(errs.CodeInvalidArgument, "from and to must differ")
	}
	if req.From == domain.RoleArchive {
		return errs.New(errs.CodeForbiddenStorageRole, "archive is read only through a restore; use /storages/restore")
	}
	ref, err := assetRefFrom(req.AssetID)
	if err != nil {
		return err
	}
	_, files, err := s.filesInRole(c, ref, req.From)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errs.Newf(errs.CodeFailedPrecondition, "asset %s has no files in the %s role", req.AssetID, req.From)
	}
	definition, err := copyWorkflow(req, files)
	if err != nil {
		return err
	}
	out, err := s.d.Workflows.Run(c.ctx, c.scope, "", api.RunWorkflowRequest{
		Definition: definition, IdempotencyKey: c.scope.IdempotencyKey,
	})
	return replyRun(c, out, err)
}

func copyWorkflow(req api.StorageCopyRequest, files []domain.MediaFile) (json.RawMessage, error) {
	nodes := make([]wfdef.NodeSpec, 0, len(files))
	for i, f := range files {
		params, err := json.Marshal(map[string]string{"source": f.URI(), "storage": string(req.To), "key": f.Key})
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, wfdef.NodeSpec{
			ID: fmt.Sprintf("copy_%d", i+1), Type: "storage.copy", TypeVersion: 1, Parameters: params,
		})
	}
	def := wfdef.Definition{
		ID:          "storage-copy",
		Version:     "1.0.0",
		Description: fmt.Sprintf("Copy files of %s from %s to %s", req.AssetID, req.From, req.To),
		Trigger:     &wfdef.Trigger{Type: wfdef.TriggerCLI},
		Nodes:       nodes,
	}
	return json.Marshal(def)
}

// handleStorageRestore shows what a restore would read until it is confirmed, then
// runs restore-archived (§3.3, I132).
func (s *server) handleStorageRestore(c *call) error {
	var req api.StorageRestoreRequest
	if err := c.decode(&req); err != nil {
		return err
	}
	if !req.To.Valid() || req.To == domain.RoleArchive {
		return errs.New(errs.CodeInvalidArgument, "to must be the master or proxy role")
	}
	ref, err := assetRefFrom(req.AssetID)
	if err != nil {
		return err
	}
	if !req.Confirm {
		preview, err := s.restorePreview(c, ref, req)
		return reply(c, http.StatusOK, preview, err)
	}
	inputs, err := json.Marshal(map[string]string{"asset_id": req.AssetID, "to": string(req.To)})
	if err != nil {
		return err
	}
	out, err := s.d.Workflows.Run(c.ctx, c.scope, "restore-archived", api.RunWorkflowRequest{
		Inputs: inputs, IdempotencyKey: c.scope.IdempotencyKey,
	})
	return replyRun(c, out, err)
}

// archiveRetrievalOperation is the cost table entry of a restore, per GiB (I132).
const archiveRetrievalOperation = "storage.archive.retrieval"

func (s *server) restorePreview(c *call, ref id.Ref, req api.StorageRestoreRequest) (api.StorageRestorePreview, error) {
	asset, files, err := s.filesInRole(c, ref, domain.RoleArchive)
	if err != nil {
		return api.StorageRestorePreview{}, err
	}
	preview := api.StorageRestorePreview{ConfirmRequired: true, AssetID: asset.ID, To: req.To}
	for _, f := range files {
		preview.ArchiveBytes += f.SizeBytes
	}
	preview.Note = "no cost estimate: the tenant has no " + archiveRetrievalOperation + " entry in its cost table"
	if s.d.TenantConfig == nil {
		return preview, nil
	}
	tenantConfig, err := s.d.TenantConfig.TenantConfig(c.ctx, c.scope.Tenant)
	if err != nil {
		return api.StorageRestorePreview{}, err
	}
	perGiB, ok := tenantConfig.Budget.PerUnitMinor[archiveRetrievalOperation]
	if !ok {
		return preview, nil
	}
	estimate := int64(float64(preview.ArchiveBytes)/float64(1<<30)*perGiB + 0.5)
	preview.EstimateMinor = &estimate
	preview.Note = "an estimate from the tenant's configured cost table, not a provider quote"
	if s.d.Budgets != nil {
		if status, err := s.d.Budgets.Status(c.ctx, c.scope); err == nil {
			preview.Currency = status.Budget.Currency
		}
	}
	return preview, nil
}

// ---- Search and approvals ----

func (s *server) handleSearch(c *call) error {
	q := api.SearchQuery{Q: c.query("q"), AssetID: c.query("asset_id"), Mode: c.query("mode")}
	if q.Q == "" {
		return errs.New(errs.CodeInvalidArgument, "q is required")
	}
	switch q.Mode {
	case "", "hybrid", "lexical", "semantic":
	default:
		return errs.New(errs.CodeInvalidArgument, "mode must be hybrid, lexical or semantic")
	}
	limits, err := c.pageQuery()
	if err != nil {
		return err
	}
	q.Limit = limits.Limit
	out, err := s.d.Search.Search(c.ctx, c.scope, q)
	if err == nil && out.Items == nil {
		out.Items = []domain.SearchHit{}
	}
	return reply(c, http.StatusOK, out, err)
}

func (s *server) approvalHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteApprovalsList: func(c *call) error {
			paging, err := c.pageQuery()
			if err != nil {
				return err
			}
			q := api.ApprovalQuery{
				PageQuery: paging,
				Status:    domain.ApprovalStatus(c.query("status")),
				Kind:      domain.ApprovalKind(c.query("kind")),
				AssetID:   c.query("asset_id"),
			}
			list, err := normalized(s.d.Approvals.List(c.ctx, c.scope, q))
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteApprovalsGet: func(c *call) error {
			out, err := s.d.Approvals.Get(c.ctx, c.scope, c.ref())
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteApprovalsApprove: func(c *call) error {
			var req api.ApproveRequest
			if err := c.decodeOptional(&req); err != nil {
				return err
			}
			out, err := s.d.Approvals.Approve(c.ctx, c.scope, c.ref(), req)
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteApprovalsReject: func(c *call) error {
			var req api.RejectRequest
			if err := c.decodeOptional(&req); err != nil {
				return err
			}
			out, err := s.d.Approvals.Reject(c.ctx, c.scope, c.ref(), req)
			return reply(c, http.StatusOK, out, err)
		},
	}
}

// ---- Publications ----

func (s *server) publicationHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RoutePublicationsCreate: func(c *call) error {
			var req api.CreatePublicationRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Publications.Request(c.ctx, c.scope, req)
			return replyRun(c, out, err)
		},
		api.RoutePublicationsList: func(c *call) error {
			paging, err := c.pageQuery()
			if err != nil {
				return err
			}
			q := api.PublicationQuery{
				PageQuery:   paging,
				AssetID:     c.query("asset_id"),
				Destination: c.query("destination"),
				Status:      domain.PublicationStatus(c.query("status")),
			}
			list, err := normalized(s.d.Publications.List(c.ctx, c.scope, q))
			return reply(c, http.StatusOK, list, err)
		},
		api.RoutePublicationsGet: func(c *call) error {
			out, err := s.d.Publications.Get(c.ctx, c.scope, c.ref())
			return reply(c, http.StatusOK, out, err)
		},
		api.RoutePublicationsWithdraw: func(c *call) error {
			out, err := s.d.Publications.WithdrawOne(c.ctx, c.scope, c.ref())
			return replyRun(c, out, err)
		},
		api.RoutePublicationsUnpub: func(c *call) error {
			var req api.WithdrawRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Publications.Withdraw(c.ctx, c.scope, req)
			return replyRun(c, out, err)
		},
		api.RoutePublicationsSync: func(c *call) error {
			out, err := s.d.Publications.Sync(c.ctx, c.scope, c.ref())
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteDestinationsList: func(c *call) error {
			items, err := s.d.Publications.Destinations(c.ctx, c.scope)
			list, err := listOf(items, err)
			return reply(c, http.StatusOK, list, err)
		},
	}
}

// ---- Introspection, plugins, events ----

func (s *server) introspectionHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteNodesList: func(c *call) error {
			items, err := s.d.Introspection.Nodes(c.ctx, c.scope)
			list, err := listOf(items, err)
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteNodesGet: func(c *call) error {
			out, err := s.d.Introspection.Node(c.ctx, c.scope, c.param("type"))
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteHooksList: func(c *call) error {
			items, err := s.d.Introspection.Hooks(c.ctx, c.scope, c.query("workflow"))
			list, err := listOf(items, err)
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteHooksTest: func(c *call) error {
			var req api.TestHookRequest
			if err := c.decodeOptional(&req); err != nil {
				return err
			}
			out, err := s.d.Introspection.TestHook(c.ctx, c.scope, c.param("id"), req)
			return reply(c, http.StatusOK, out, err)
		},
	}
}

func (s *server) pluginHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RoutePluginsList: func(c *call) error {
			items, err := s.d.Plugins.List(c.ctx, c.scope)
			list, err := listOf(items, err)
			return reply(c, http.StatusOK, list, err)
		},
		api.RoutePluginsGet: func(c *call) error {
			out, err := s.d.Plugins.Get(c.ctx, c.scope, c.param("id"))
			return reply(c, http.StatusOK, out, err)
		},
		api.RoutePluginsAdd: func(c *call) error {
			var req api.InstallPluginRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Plugins.Install(c.ctx, c.scope, req)
			return reply(c, http.StatusCreated, out, err)
		},
	}
}

func (s *server) eventQuery(c *call) (api.EventQuery, error) {
	paging, err := c.pageQuery()
	if err != nil {
		return api.EventQuery{}, err
	}
	return api.EventQuery{PageQuery: paging, Since: c.query("since"), Type: event.Type(c.query("type"))}, nil
}

func (s *server) eventHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteEventsList: func(c *call) error {
			q, err := s.eventQuery(c)
			if err != nil {
				return err
			}
			list, err := normalized(s.d.Events.List(c.ctx, c.scope, q))
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteEventsDeliv: func(c *call) error {
			paging, err := c.pageQuery()
			if err != nil {
				return err
			}
			q := api.DeliveryQuery{PageQuery: paging, Status: event.DeliveryStatus(c.query("status"))}
			list, err := normalized(s.d.Events.Deliveries(c.ctx, c.scope, q))
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteEventsStream: func(c *call) error {
			q, err := s.eventQuery(c)
			if err != nil {
				return err
			}
			after, err := c.lastEventID()
			if err != nil {
				return err
			}
			ctx, cancel := subscription(c)
			defer cancel()
			ch, err := s.d.Events.Subscribe(ctx, c.scope, q, after)
			if err != nil {
				return err
			}
			return serveSSE(s, c, ch, func(e service.StreamedEvent) frame {
				return frame{id: seqID(e.Seq), event: api.SSEEvent, data: e.Event}
			})
		},
	}
}

// ---- Secrets ----

var secretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// secretHandlers only ever returns metadata (R41). Client-location secrets never
// reach this route (§9.1).
func (s *server) secretHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteSecretsList: func(c *call) error {
			items, err := s.d.Secrets.List(c.ctx, c.scope)
			list, err := listOf(items, err)
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteSecretsPut: func(c *call) error {
			var req api.PutSecretRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			if !secretName.MatchString(req.Name) {
				return errs.New(errs.CodeInvalidArgument, "name must be a reference name, up to 128 characters")
			}
			if req.Location == domain.SecretClient {
				return errs.New(errs.CodeInvalidArgument, "client-location secrets stay in the client's keyring and never reach the server")
			}
			if req.Value == "" {
				return errs.New(errs.CodeInvalidArgument, "value is required")
			}
			out, err := s.d.Secrets.Put(c.ctx, c.scope, req)
			return reply(c, http.StatusCreated, out, err)
		},
		api.RouteSecretsDelete: func(c *call) error {
			return reply(c, http.StatusOK, empty, s.d.Secrets.Delete(c.ctx, c.scope, c.param("name")))
		},
		api.RouteSecretsTest: func(c *call) error {
			out, err := s.d.Secrets.Test(c.ctx, c.scope, c.param("name"))
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteSecretsRotate: func(c *call) error {
			var req api.RotateSecretRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			if req.Value == "" {
				return errs.New(errs.CodeInvalidArgument, "value is required")
			}
			out, err := s.d.Secrets.Rotate(c.ctx, c.scope, c.param("name"), req)
			return reply(c, http.StatusOK, out, err)
		},
	}
}

// ---- Pairing and devices ----

func (s *server) deviceHandlers() map[string]handlerFunc {
	list := func(c *call) error {
		items, err := s.d.Devices.List(c.ctx, c.scope)
		out, err := listOf(items, err)
		return reply(c, http.StatusOK, out, err)
	}
	revoke := func(c *call) error {
		out, err := s.d.Devices.Revoke(c.ctx, c.scope, c.ref())
		return reply(c, http.StatusOK, out, err)
	}
	return map[string]handlerFunc{
		api.RoutePairingCodes: func(c *call) error {
			var req api.CreatePairingCodeRequest
			if err := c.decodeOptional(&req); err != nil {
				return err
			}
			out, err := s.d.Devices.CreatePairingCode(c.ctx, c.scope, req)
			if err == nil && out.TLSFingerprint == "" {
				out.TLSFingerprint = s.d.TLSFingerprint
			}
			return reply(c, http.StatusCreated, out, err)
		},
		api.RouteDevicesPair: func(c *call) error {
			if ok, wait := s.pairLimit.allow(c.remoteKey()); !ok {
				return errs.New(errs.CodeRateLimited, "too many pairing attempts; try again later").
					With("retry_after_seconds", retryAfter(wait))
			}
			var req api.PairDeviceRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			if req.Code == "" {
				return errs.New(errs.CodeInvalidArgument, "code is required")
			}
			out, err := s.d.Devices.Pair(c.ctx, req)
			return reply(c, http.StatusCreated, out, err)
		},
		api.RouteDevicesList:   list,
		api.RouteClientsList:   list,
		api.RouteDevicesRevoke: revoke,
		api.RouteClientsRevoke: revoke,
		api.RouteDevicesRotate: func(c *call) error {
			out, err := s.d.Devices.Rotate(c.ctx, c.scope, c.ref())
			return reply(c, http.StatusOK, out, err)
		},
	}
}

// ---- Tenants, users, budget, audit ----

func (s *server) tenantHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteTenantsList: func(c *call) error {
			items, err := s.d.Tenants.List(c.ctx, c.scope.Principal)
			list, err := listOf(items, err)
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteTenantsAdd: func(c *call) error {
			var req api.CreateTenantRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			if err := req.ID.Validate(); err != nil {
				return errs.New(errs.CodeInvalidArgument, "id must be a tenant slug")
			}
			out, err := s.d.Tenants.Create(c.ctx, c.scope.Principal, req)
			return reply(c, http.StatusCreated, out, err)
		},
	}
}

func (s *server) userHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteUsersList: func(c *call) error {
			items, err := s.d.Users.List(c.ctx, c.scope)
			list, err := listOf(items, err)
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteUsersAdd: func(c *call) error {
			var req api.AddUserRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			if !req.Role.Valid() {
				return errs.New(errs.CodeInvalidArgument, "role must be viewer, editor, publisher or admin")
			}
			out, err := s.d.Users.Add(c.ctx, c.scope, req)
			return reply(c, http.StatusCreated, out, err)
		},
		api.RouteUsersGrant: func(c *call) error {
			var req api.GrantRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			if req.Permission == "" || req.Permission.SystemLevel() {
				return errs.New(errs.CodeInvalidArgument, "permission must be a tenant permission")
			}
			out, err := s.d.Users.Grant(c.ctx, c.scope, c.param("id"), req)
			return reply(c, http.StatusOK, out, err)
		},
	}
}

func (s *server) budgetHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteBudgetGet: func(c *call) error {
			out, err := s.d.Budgets.Status(c.ctx, c.scope)
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteBudgetPut: func(c *call) error {
			var req api.SetBudgetRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Budgets.Set(c.ctx, c.scope, req)
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteBudgetOver: func(c *call) error {
			var req api.BudgetOverrideRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			if strings.TrimSpace(req.Reason) == "" {
				return errs.New(errs.CodeInvalidArgument, "reason is required")
			}
			out, err := s.d.Budgets.Override(c.ctx, c.scope, req)
			return reply(c, http.StatusOK, out, err)
		},
	}
}

func (s *server) auditHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteAuditList: func(c *call) error {
			paging, err := c.pageQuery()
			if err != nil {
				return err
			}
			q := api.AuditQuery{
				PageQuery: paging,
				Since:     c.query("since"),
				Actor:     c.query("actor"),
				Action:    c.query("action"),
				Resource:  c.query("resource"),
			}
			list, err := normalized(s.d.Audit.List(c.ctx, c.scope, q))
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteAuditVerify: func(c *call) error {
			out, err := s.d.Audit.Verify(c.ctx, c.scope)
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteAuditExport: func(c *call) error {
			out, err := s.d.Audit.Export(c.ctx, c.scope)
			return reply(c, http.StatusOK, out, err)
		},
	}
}

// ---- Logs and observability ----

func (s *server) logQuery(c *call) (api.LogQuery, error) {
	paging, err := c.pageQuery()
	if err != nil {
		return api.LogQuery{}, err
	}
	return api.LogQuery{PageQuery: paging, Since: c.query("since"), Level: c.query("level"), JobID: c.query("job_id")}, nil
}

func (s *server) observabilityHandlers() map[string]handlerFunc {
	sinks := func(c *call) error {
		items, err := s.d.Observability.Sinks(c.ctx, c.scope)
		list, err := listOf(items, err)
		return reply(c, http.StatusOK, list, err)
	}
	setEnabled := func(enabled bool) handlerFunc {
		return func(c *call) error {
			return reply(c, http.StatusOK, empty, s.d.Observability.SetSinkEnabled(c.ctx, c.scope, c.param("id"), enabled))
		}
	}
	return map[string]handlerFunc{
		api.RouteMetrics: s.handleMetrics,
		api.RouteLogsList: func(c *call) error {
			q, err := s.logQuery(c)
			if err != nil {
				return err
			}
			list, err := normalized(s.d.Observability.Logs(c.ctx, c.scope, q))
			return reply(c, http.StatusOK, list, err)
		},
		api.RouteLogsStream: func(c *call) error {
			q, err := s.logQuery(c)
			if err != nil {
				return err
			}
			ctx, cancel := subscription(c)
			defer cancel()
			ch, err := s.d.Observability.SubscribeLogs(ctx, c.scope, q)
			if err != nil {
				return err
			}
			return serveSSE(s, c, ch, func(r connector.LogRecord) frame {
				return frame{event: api.SSELog, data: r}
			})
		},
		api.RouteObsList:   sinks,
		api.RouteObsStatus: sinks,
		api.RouteObsTest: func(c *call) error {
			return reply(c, http.StatusOK, empty, s.d.Observability.TestSink(c.ctx, c.scope, c.param("id")))
		},
		api.RouteObsEnable:  setEnabled(true),
		api.RouteObsDisable: setEnabled(false),
	}
}

func (s *server) handleDoctor(c *call) error {
	check := c.query("check")
	switch check {
	case "", "security", "network", "storage":
	default:
		return errs.New(errs.CodeInvalidArgument, "check must be security, network or storage")
	}
	out, err := s.d.Doctor.Run(c.ctx, c.scope, check)
	return reply(c, http.StatusOK, out, err)
}

// ---- Worker protocol (I4) ----

func (s *server) workerHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		api.RouteWorkerRegister: func(c *call) error {
			var req workerproto.RegisterRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Coordinator.Register(c.ctx, req)
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteWorkerHeartbeat: func(c *call) error {
			var req workerproto.HeartbeatRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			return reply(c, http.StatusOK, empty, s.d.Coordinator.Heartbeat(c.ctx, req))
		},
		api.RouteWorkerLease: func(c *call) error {
			var req workerproto.LeaseRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Coordinator.Lease(c.ctx, c.param("id"), req)
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteWorkerTaskHeartbeat: func(c *call) error {
			var req workerproto.TaskHeartbeatRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Coordinator.TaskHeartbeat(c.ctx, c.param("id"), req)
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteWorkerTaskLogs: func(c *call) error {
			var req workerproto.TaskLogsRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			return reply(c, http.StatusOK, empty, s.d.Coordinator.TaskLogs(c.ctx, c.param("id"), req))
		},
		api.RouteWorkerComplete: func(c *call) error {
			var req workerproto.CompleteRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			if len(req.Output) > domain.MaxNodeOutputBytes {
				return errs.Newf(errs.CodeOutputTooLarge, "node output exceeds %d bytes", domain.MaxNodeOutputBytes)
			}
			return reply(c, http.StatusOK, empty, s.d.Coordinator.Complete(c.ctx, c.param("id"), req))
		},
		api.RouteWorkerFail: func(c *call) error {
			var req workerproto.FailRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			return reply(c, http.StatusOK, empty, s.d.Coordinator.Fail(c.ctx, c.param("id"), req))
		},
		api.RouteWorkerConnection: func(c *call) error {
			var req workerproto.ConnectionRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Coordinator.Connection(c.ctx, c.param("id"), req)
			return reply(c, http.StatusOK, out, err)
		},
		api.RouteWorkerCacheKey: func(c *call) error {
			var req workerproto.CacheKeyRequest
			if err := c.decode(&req); err != nil {
				return err
			}
			out, err := s.d.Coordinator.CacheKey(c.ctx, c.param("id"), req)
			return reply(c, http.StatusOK, out, err)
		},
	}
}
