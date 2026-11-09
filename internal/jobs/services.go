package jobs

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/internal/events"
	"github.com/rafaelp/vidyom/internal/hooks"
	"github.com/rafaelp/vidyom/internal/scheduler"
	"github.com/rafaelp/vidyom/internal/workflow"
	"github.com/rafaelp/vidyom/schemas"
)

// Workflows returns service.Workflows.
func (e *Engine) Workflows() service.Workflows { return workflowsService{e} }

// Jobs returns service.Jobs.
func (e *Engine) Jobs() service.Jobs { return jobsService{e} }

// Approvals returns service.Approvals.
func (e *Engine) Approvals() service.Approvals { return approvalsService{e} }

// Introspection returns service.Introspection.
func (e *Engine) Introspection() service.Introspection { return introspectionService{e} }

func (e *Engine) tenantStore(ctx context.Context, s service.Scope) (repo.TenantStore, error) {
	if s.Tenant == "" {
		return nil, errs.New(errs.CodeTenantRequired, "this operation needs a tenant")
	}
	return e.d.Tenants.ForTenant(ctx, s.Tenant)
}

// ---- creating jobs ----

type jobSpec struct {
	def         *wfdef.Definition
	snapshot    []byte
	inputs      map[string]any
	key         string
	submittedBy string
	permissions []domain.Permission
	traceID     string
	trigger     domain.JobTrigger
	assetID     id.Friendly
}

// createJob commits a job with a pending record per node. A known idempotency key
// returns the existing job, and false.
func (e *Engine) createJob(ctx context.Context, store repo.TenantStore, spec jobSpec) (domain.Job, bool, error) {
	if spec.key != "" {
		existing, found, err := store.Jobs().FindByIdempotencyKey(ctx, spec.key)
		if err != nil {
			return domain.Job{}, false, err
		}
		if found {
			return existing, false, nil
		}
	}
	inputs, err := workflow.EncodeJSON(spec.inputs)
	if err != nil {
		return domain.Job{}, false, errs.Wrap(errs.CodeInvalidArgument, err, "inputs do not encode")
	}
	trace := spec.traceID
	if trace == "" {
		trace = newTraceID()
	}
	asset := spec.assetID
	if asset == "" {
		asset = e.assetFromInputs(ctx, store, spec.inputs)
	}
	now := e.now()
	job := domain.Job{
		ULID:            id.New(),
		TenantID:        store.Tenant(),
		WorkflowID:      spec.def.ID,
		WorkflowVersion: spec.def.Version,
		WorkflowSHA256:  wfdef.SHA256(spec.snapshot),
		Snapshot:        spec.snapshot,
		Status:          domain.JobQueued,
		Inputs:          inputs,
		Trigger:         spec.trigger,
		AssetID:         asset,
		SubmittedBy:     spec.submittedBy,
		Permissions:     spec.permissions,
		IdempotencyKey:  spec.key,
		TraceID:         trace,
		CreatedAt:       now,
	}
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		friendly, err := tx.IDs().Next(ctx, id.PrefixJob)
		if err != nil {
			return err
		}
		job.ID = friendly
		if err := tx.Jobs().Create(ctx, job); err != nil {
			return err
		}
		for i := range spec.def.Nodes {
			n := &spec.def.Nodes[i]
			if err := tx.Jobs().PutNode(ctx, domain.NodeExecution{
				JobULID: job.ULID, NodeID: n.ID, NodeType: n.Type, TypeVersion: n.TypeVersion, Status: domain.NodePending,
			}); err != nil {
				return err
			}
		}
		data, _ := json.Marshal(map[string]any{"status": job.Status})
		_, err = tx.Jobs().AppendEvent(ctx, domain.JobEvent{JobID: job.ID, At: now, Kind: domain.JobEventState, Data: data})
		return err
	})
	if err != nil {
		if spec.key != "" && (errs.IsCode(err, errs.CodeConflict) || errs.IsCode(err, errs.CodeIdempotencyMismatch)) {
			if existing, found, findErr := store.Jobs().FindByIdempotencyKey(ctx, spec.key); findErr == nil && found {
				return existing, false, nil
			}
		}
		return domain.Job{}, false, err
	}
	e.tenants.Store(job.ULID, store.Tenant())
	e.Kick(store.Tenant(), job.ULID)
	return job, true, nil
}

// assetFromInputs names the job's asset when an input does (§12.1, JobQuery).
func (e *Engine) assetFromInputs(ctx context.Context, store repo.TenantStore, inputs map[string]any) id.Friendly {
	for _, name := range []string{"asset_id", "assetId", "source_id"} {
		if s, ok := inputs[name].(string); ok && s != "" {
			if a, err := store.Assets().Get(ctx, id.Ref(s)); err == nil {
				return a.ID
			}
		}
	}
	return ""
}

// StartEventJob implements events.JobStarter: a job with no client connected, whose
// events continue the chain (§8.2, I156).
func (e *Engine) StartEventJob(ctx context.Context, req events.StartRequest) error {
	return e.startSystemJob(ctx, req.Tenant, req.WorkflowID, req.Version, req.IdempotencyKey, domain.JobTrigger{
		Type: "event", EventID: string(req.Event.EventID), CausationID: string(req.Event.EventID), Depth: req.Event.CausationDepth + 1,
	}, req.Event.TraceID)
}

// StartScheduledJob implements scheduler.JobStarter.
func (e *Engine) StartScheduledJob(ctx context.Context, req scheduler.StartRequest) error {
	return e.startSystemJob(ctx, req.Tenant, req.WorkflowID, req.Version, req.IdempotencyKey, domain.JobTrigger{Type: "schedule"}, "")
}

var (
	_ events.JobStarter    = (*Engine)(nil)
	_ scheduler.JobStarter = (*Engine)(nil)
)

func (e *Engine) startSystemJob(ctx context.Context, tenant domain.TenantID, workflowID, version, key string, trigger domain.JobTrigger, trace string) error {
	entry, ok := e.d.Workflows.Get(workflowID, version)
	if !ok {
		return errs.Newf(errs.CodeNotFound, "workflow %s is not available", workflowID)
	}
	store, err := e.d.Tenants.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	perms := workflow.RequiredPermissions(entry.Definition, e.d.Workflows)
	for _, p := range perms {
		if workflow.InteractiveOnly[p] {
			return errs.Newf(errs.CodeForbidden, "workflow %s needs %s, which only a person can start (I157)", workflowID, p)
		}
	}
	inputs, err := workflow.BindInputs(entry.Definition, nil, workflow.BindOptions{})
	if err != nil {
		return err
	}
	snapshot, err := workflow.BuildSnapshot(entry.Raw, workflow.RawFromRegistry(e.d.Workflows))
	if err != nil {
		return err
	}
	_, _, err = e.createJob(ctx, store, jobSpec{
		def: entry.Definition, snapshot: snapshot, inputs: inputs, key: key,
		submittedBy: string(domain.AuthSystem), permissions: perms, traceID: trace, trigger: trigger,
	})
	return err
}

// ---- service.Workflows ----

type workflowsService struct{ e *Engine }

var _ service.Workflows = workflowsService{}

func summaryOf(e *workflow.Entry) api.WorkflowSummary {
	d := e.Definition
	return api.WorkflowSummary{ID: d.ID, Name: d.Name, Version: d.Version, Description: d.Description, Inputs: d.Inputs, Trigger: d.Trigger, SHA256: e.SHA256}
}

// List implements service.Workflows: only what the caller may run (D59).
func (w workflowsService) List(_ context.Context, s service.Scope) ([]api.WorkflowSummary, error) {
	out := []api.WorkflowSummary{}
	for _, entry := range w.e.d.Workflows.List() {
		permitted := true
		for _, p := range workflow.RequiredPermissions(entry.Definition, w.e.d.Workflows) {
			if !s.Principal.Can(p) {
				permitted = false
				break
			}
		}
		if permitted {
			out = append(out, summaryOf(entry))
		}
	}
	return out, nil
}

// Get implements service.Workflows.
func (w workflowsService) Get(_ context.Context, _ service.Scope, workflowID, version string) (api.WorkflowDetail, error) {
	entry, ok := w.e.d.Workflows.Get(workflowID, version)
	if !ok {
		return api.WorkflowDetail{}, errs.NotFound("workflow")
	}
	return api.WorkflowDetail{WorkflowSummary: summaryOf(entry), Definition: entry.Raw}, nil
}

// Validate implements service.Workflows: static rules, and the R37 warning with what
// the tenant has.
func (w workflowsService) Validate(ctx context.Context, s service.Scope, req api.ValidateWorkflowRequest) (api.ValidationResult, error) {
	res := w.e.d.Validator.Validate(req.Definition)
	if res.OK() {
		env, err := w.dryRunEnv(ctx, s)
		if err != nil {
			return api.ValidationResult{}, err
		}
		env.SkipPermissions = true
		res.Issues = append(res.Issues, w.e.d.Validator.DryRun(res, env)...)
	}
	return res.API(), nil
}

func (w workflowsService) dryRunEnv(ctx context.Context, s service.Scope) (workflow.DryRunEnv, error) {
	e := w.e
	env := workflow.DryRunEnv{Principal: s.Principal, Workers: e.d.Scheduler.Matcher.List(), WorkersKnown: true}
	if e.d.Config != nil {
		cfg, err := e.d.Config.TenantConfig(ctx, s.Tenant)
		if err != nil && !errs.IsCode(err, errs.CodeNotFound) {
			return env, err
		}
		if err == nil {
			env.Tenant = &cfg
		}
	}
	if e.d.Connectors != nil {
		env.Connectors = e.d.Connectors.List()
	}
	store, err := e.tenantStore(ctx, s)
	if err != nil {
		return env, err
	}
	secrets, err := store.Secrets().List(ctx)
	if err != nil {
		return env, err
	}
	env.Secrets, env.SecretsKnown = secrets, true
	if e.d.Plugins != nil {
		plugins, err := e.d.Plugins.List(ctx)
		if err != nil {
			return env, err
		}
		env.Plugins, env.PluginsKnown = plugins, true
	}
	return env, nil
}

// Run implements service.Workflows. It returns as soon as the job exists; clients
// attach later, or never (§5.5).
func (w workflowsService) Run(ctx context.Context, s service.Scope, workflowID string, req api.RunWorkflowRequest) (api.RunWorkflowResponse, error) {
	e := w.e
	store, err := e.tenantStore(ctx, s)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	if req.DryRun && workflowID == "" && len(req.Definition) > 0 {
		// A dry run reports what is wrong with a definition; it does not refuse it.
		res := e.d.Validator.Validate(req.Definition)
		if res.Definition == nil {
			result := res.API()
			return api.RunWorkflowResponse{Validation: &result}, nil
		}
		return w.dryRun(ctx, s, store, req.Definition, res.Definition, req)
	}
	raw, def, err := w.definition(workflowID, req)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	if req.DryRun {
		return w.dryRun(ctx, s, store, raw, def, req)
	}
	if def.Trigger != nil && def.Trigger.Type == wfdef.TriggerWebhook {
		return api.RunWorkflowResponse{}, errs.NotImplemented("webhook triggers")
	}
	key := s.IdempotencyKey
	if req.IdempotencyKey != "" {
		if key != "" && key != req.IdempotencyKey {
			return api.RunWorkflowResponse{}, errs.New(errs.CodeInvalidArgument, "the Idempotency-Key header and idempotency_key differ")
		}
		key = req.IdempotencyKey
	}
	if key != "" {
		existing, found, err := store.Jobs().FindByIdempotencyKey(ctx, key)
		if err != nil {
			return api.RunWorkflowResponse{}, err
		}
		if found {
			if existing.WorkflowID != def.ID {
				return api.RunWorkflowResponse{}, errs.Newf(errs.CodeIdempotencyMismatch, "this idempotency key started workflow %s", existing.WorkflowID)
			}
			return responseOf(existing), nil
		}
	}
	perms, err := e.grantPermissions(def, s.Principal)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	inputs, err := workflow.BindInputs(def, req.Inputs, workflow.BindOptions{File: e.fileInput(ctx, store, s.Principal)})
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	snapshot, err := workflow.BuildSnapshot(raw, workflow.RawFromRegistry(e.d.Workflows))
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	job, _, err := e.createJob(ctx, store, jobSpec{
		def: def, snapshot: snapshot, inputs: inputs, key: key, submittedBy: s.Principal.Actor(),
		permissions: perms, traceID: s.TraceID, trigger: domain.JobTrigger{Type: "cli"},
	})
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	return responseOf(job), nil
}

func responseOf(j domain.Job) api.RunWorkflowResponse {
	return api.RunWorkflowResponse{JobID: j.ID, JobULID: j.ULID, Status: j.Status, Workflow: j.WorkflowID, WorkflowVersion: j.WorkflowVersion}
}

// definition finds what to run: a registered workflow, or an inline definition, which
// goes through the same validation and permissions (I22, §9.4).
func (w workflowsService) definition(workflowID string, req api.RunWorkflowRequest) ([]byte, *wfdef.Definition, error) {
	e := w.e
	if workflowID == "" {
		if len(req.Definition) == 0 {
			return nil, nil, errs.New(errs.CodeInvalidArgument, "name a workflow or send a definition")
		}
		res := e.d.Validator.Validate(req.Definition)
		if err := res.Err(); err != nil {
			return nil, nil, err
		}
		return req.Definition, res.Definition, nil
	}
	entry, ok := e.d.Workflows.Get(workflowID, req.WorkflowVersion)
	if !ok {
		return nil, nil, errs.NotFound("workflow")
	}
	return entry.Raw, entry.Definition, nil
}

// grantPermissions requires what the workflow tree needs and returns exactly that as
// the job's permissions (I157).
func (e *Engine) grantPermissions(def *wfdef.Definition, p domain.Principal) ([]domain.Permission, error) {
	perms := workflow.RequiredPermissions(def, e.d.Workflows)
	for _, perm := range perms {
		if !p.Can(perm) {
			return nil, errs.Forbidden(string(perm))
		}
	}
	if p.Can(domain.PermAssetAIConsent) {
		// catalog.register's aiProcessing input records this person's consent (D8).
		perms = append(perms, domain.PermAssetAIConsent)
	}
	return perms, nil
}

// fileInput is the second layer of R20, after the API's: it also serves callers that
// do not come through it, such as publishing's service.
func (e *Engine) fileInput(ctx context.Context, store repo.TenantStore, p domain.Principal) func(name, value string) (string, error) {
	return func(name, value string) (string, error) {
		u, err := domain.ParseUserInputURI(value)
		if err != nil {
			return "", errs.Wrap(errs.CodeInvalidArgument, err, "input "+name+": "+err.Error())
		}
		switch u.Kind {
		case domain.URIServer:
			path, err := domain.ResolveServerPath(e.d.Daemon.IngestRoots, u.Path)
			if err != nil {
				return "", err
			}
			u.Path = path
		case domain.URIUpload:
			upload, err := store.Uploads().Get(ctx, id.Ref(u.UploadRef))
			if err != nil {
				return "", err
			}
			if upload.Status != domain.UploadComplete {
				return "", errs.Newf(errs.CodeFailedPrecondition, "upload %s is not complete", upload.ID)
			}
			u.UploadRef = string(upload.ULID)
		case domain.URIStorage:
			if u.Role == domain.RoleArchive && !p.Can(domain.PermStorageArchiveRestore) {
				return "", errs.Forbidden(string(domain.PermStorageArchiveRestore))
			}
		}
		return u.String(), nil
	}
}

// dryRun validates syntax, schemas, connectors, permissions, resources and order with
// no external effect (§6.2).
func (w workflowsService) dryRun(ctx context.Context, s service.Scope, store repo.TenantStore, raw []byte, def *wfdef.Definition, req api.RunWorkflowRequest) (api.RunWorkflowResponse, error) {
	e := w.e
	res := e.d.Validator.Validate(raw)
	if res.OK() {
		env, err := w.dryRunEnv(ctx, s)
		if err != nil {
			return api.RunWorkflowResponse{}, err
		}
		res.Issues = append(res.Issues, e.d.Validator.DryRun(res, env)...)
	}
	if res.Definition != nil {
		given := map[string]any{}
		if len(req.Inputs) > 0 {
			_ = json.Unmarshal(req.Inputs, &given)
		}
		_, err := workflow.BindInputs(res.Definition, req.Inputs, workflow.BindOptions{File: e.fileInput(ctx, store, s.Principal), AllowMissing: true})
		if err != nil {
			res.Issues = append(res.Issues, api.ValidationIssue{Severity: workflow.SeverityError, Code: workflow.IssueInput, Message: errs.Public(err).Message})
		}
		for name, spec := range res.Definition.Inputs {
			if _, ok := given[name]; !ok && spec.Required && len(spec.Default) == 0 {
				res.Issues = append(res.Issues, api.ValidationIssue{
					Severity: workflow.SeverityWarning, Code: workflow.IssueInput, Path: "inputs." + name,
					Message: "input " + name + " is required when this workflow runs",
				})
			}
		}
	}
	result := res.API()
	return api.RunWorkflowResponse{Workflow: def.ID, WorkflowVersion: def.Version, Validation: &result}, nil
}

// RunNode implements service.Workflows: `vidyom node run`, an ad-hoc one-node job.
// Control nodes make no sense alone, and D53 refuses video.cut (I142).
func (w workflowsService) RunNode(ctx context.Context, s service.Scope, nodeType string, req api.RunNodeRequest) (api.RunWorkflowResponse, error) {
	e := w.e
	store, err := e.tenantStore(ctx, s)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	version := latestTypeVersion(e, nodeType)
	if version == 0 {
		return api.RunWorkflowResponse{}, errs.NotFound("node type")
	}
	desc, _ := e.descriptor(nodeType, version)
	if desc.Pool == node.PoolEngine {
		return api.RunWorkflowResponse{}, errs.Newf(errs.CodeInvalidArgument, "%s only makes sense inside a workflow", nodeType)
	}
	var asset domain.Asset
	if req.AssetID != "" {
		asset, err = store.Assets().Get(ctx, id.Ref(req.AssetID))
		if err != nil {
			return api.RunWorkflowResponse{}, err
		}
	}
	params, err := fillParameters(desc, req.Parameters, asset)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	doc := map[string]any{
		"id": "node-run-" + strings.ReplaceAll(strings.ReplaceAll(nodeType, ".", "-"), "_", "-"), "version": "1.0.0",
		"nodes": []map[string]any{{"id": "run", "type": nodeType, "typeVersion": version, "parameters": params}},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	res := e.d.Validator.Validate(raw)
	if err := res.Err(); err != nil {
		return api.RunWorkflowResponse{}, err
	}
	if err := workflow.ValidateResolved(desc, mustEncode(params)); err != nil && !containsRef(params) {
		return api.RunWorkflowResponse{}, err
	}
	perms, err := e.grantPermissions(res.Definition, s.Principal)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	job, _, err := e.createJob(ctx, store, jobSpec{
		def: res.Definition, snapshot: raw, inputs: map[string]any{}, key: s.IdempotencyKey, submittedBy: s.Principal.Actor(),
		permissions: perms, traceID: s.TraceID, trigger: domain.JobTrigger{Type: "cli"}, assetID: asset.ID,
	})
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	return responseOf(job), nil
}

func mustEncode(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

func containsRef(v map[string]any) bool {
	found := false
	_ = workflow.CollectRefs(any(v), nil, func(workflow.Ref) { found = true })
	return found
}

func latestTypeVersion(e *Engine, nodeType string) int {
	best := 0
	if e.d.Nodes != nil {
		for _, d := range e.d.Nodes.List() {
			if d.Type == nodeType && d.TypeVersion > best {
				best = d.TypeVersion
			}
		}
	}
	if best > 0 {
		return best
	}
	if cat, err := schemas.NodeCatalog(); err == nil {
		for _, c := range cat.Nodes {
			if c.Type == nodeType && c.TypeVersion > best {
				best = c.TypeVersion
			}
		}
	}
	return best
}

// fillParameters gives `node run --asset ORI-001` what it means: the asset's ID, and
// its working master as the source, never the archive (§3.2).
func fillParameters(desc node.Descriptor, raw json.RawMessage, asset domain.Asset) (map[string]any, error) {
	params := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, errs.Wrap(errs.CodeInvalidArgument, err, "parameters must be a JSON object")
		}
	}
	if asset.ID == "" {
		return params, nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal(desc.Parameters, &schema)
	if _, declared := schema.Properties["assetId"]; declared {
		if _, set := params["assetId"]; !set {
			params["assetId"] = string(asset.ID)
		}
	}
	if _, declared := schema.Properties["source"]; declared {
		if _, set := params["source"]; !set && asset.WorkingMaster != nil {
			params["source"] = asset.WorkingMaster.URI()
		}
	}
	return params, nil
}

// CreateClip implements service.Workflows: a thin wrapper over create-clip, which
// always has its selection gate (D53). A source from another tenant answers 404 here
// and not as a failed job.
func (w workflowsService) CreateClip(ctx context.Context, s service.Scope, req api.CreateClipRequest) (api.RunWorkflowResponse, error) {
	store, err := w.e.tenantStore(ctx, s)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	if req.SourceID == "" || req.Prompt == "" {
		return api.RunWorkflowResponse{}, errs.New(errs.CodeInvalidArgument, "a clip needs source_id and prompt")
	}
	source, err := store.Assets().Get(ctx, id.Ref(req.SourceID))
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	inputs := map[string]any{"source_id": string(source.ID), "prompt": req.Prompt}
	if req.Mode != "" {
		inputs["mode"] = string(req.Mode)
	}
	if req.TargetDurationSeconds > 0 {
		inputs["target_duration_seconds"] = req.TargetDurationSeconds
	}
	if len(req.Destinations) > 0 {
		inputs["destinations"] = req.Destinations
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	return w.Run(ctx, s, "create-clip", api.RunWorkflowRequest{WorkflowVersion: req.WorkflowVersion, Inputs: raw})
}

// ---- service.Jobs ----

type jobsService struct{ e *Engine }

var _ service.Jobs = jobsService{}

func nonNilList[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

// List implements service.Jobs.
func (j jobsService) List(ctx context.Context, s service.Scope, q api.JobQuery) (api.List[domain.Job], error) {
	store, err := j.e.tenantStore(ctx, s)
	if err != nil {
		return api.List[domain.Job]{}, err
	}
	items, next, err := store.Jobs().List(ctx, repo.JobFilter{Status: q.Status, WorkflowID: q.Workflow, AssetID: id.Ref(q.AssetID)}, repo.Page{Limit: q.Limit, Cursor: q.Cursor})
	if err != nil {
		return api.List[domain.Job]{}, err
	}
	for i := range items {
		items[i].Snapshot = nil
	}
	return api.List[domain.Job]{Items: nonNilList(items), NextCursor: next}, nil
}

// Get implements service.Jobs. What a waiting node keeps in Output is internal (I146).
func (j jobsService) Get(ctx context.Context, s service.Scope, ref id.Ref) (api.JobDetail, error) {
	store, err := j.e.tenantStore(ctx, s)
	if err != nil {
		return api.JobDetail{}, err
	}
	job, err := store.Jobs().Get(ctx, ref)
	if err != nil {
		return api.JobDetail{}, err
	}
	nodes, err := store.Jobs().ListNodes(ctx, job.ULID)
	if err != nil {
		return api.JobDetail{}, err
	}
	for i := range nodes {
		if nodes[i].Status != domain.NodeCompleted {
			nodes[i].Output = nil
		}
	}
	return api.JobDetail{Job: job, Nodes: nodes}, nil
}

// Cancel implements service.Jobs. It is idempotent. needs_attention is refused: job
// resolve is the only way out (D11).
func (j jobsService) Cancel(ctx context.Context, s service.Scope, ref id.Ref, req api.JobActionRequest) (domain.Job, error) {
	store, err := j.e.tenantStore(ctx, s)
	if err != nil {
		return domain.Job{}, err
	}
	job, err := store.Jobs().Get(ctx, ref)
	if err != nil {
		return domain.Job{}, err
	}
	switch job.Status {
	case domain.JobCancelled:
		return job, nil
	case domain.JobCompleted, domain.JobFailed:
		return domain.Job{}, errs.Newf(errs.CodeFailedPrecondition, "job %s is already %s", job.ID, job.Status)
	case domain.JobNeedsAttention:
		return domain.Job{}, errs.Newf(errs.CodeFailedPrecondition, "job %s needs attention; job resolve is the only way out (D11)", job.ID)
	}
	if err := j.e.requestCancel(ctx, store, job.ULID, req.Reason, s.Principal.Actor()); err != nil {
		return domain.Job{}, err
	}
	return store.Jobs().GetByULID(ctx, job.ULID)
}

// requestCancel marks a job for cooperative cancellation (D11). The running node
// finishes, or aborts if it can; the job ends when nothing runs.
func (e *Engine) requestCancel(ctx context.Context, store repo.TenantStore, ulid id.ULID, reason, by string) error {
	unlock := e.locks.lock(ulid)
	defer unlock()
	job, err := store.Jobs().GetByULID(ctx, ulid)
	if err != nil {
		return err
	}
	if job.Status.Terminal() || strings.HasPrefix(job.StatusReason, reasonCancelRequested) {
		return nil
	}
	job.StatusReason = reasonCancelRequested
	if reason != "" {
		job.StatusReason += ": " + reason
	}
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Jobs().Update(ctx, job); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]any{"status": job.Status, "cancel_requested_by": by})
		_, err := tx.Jobs().AppendEvent(ctx, domain.JobEvent{JobID: job.ID, At: e.now(), Kind: domain.JobEventState, Message: job.StatusReason, Data: data})
		return err
	})
	if err != nil {
		return err
	}
	e.notifyJob(ulid)
	e.cancelTasks(ctx, store, job)
	if rt, err := e.runtime(job); err == nil {
		r := &jobRun{e: e, ctx: ctx, store: store, job: job, rt: rt, decoded: map[string]any{}}
		if chain := r.chain(hook.WorkflowCancel, nil); !hooks.Empty(chain) {
			base := r.invocation(nil)
			e.spawn(func(ctx context.Context) {
				if _, err := e.d.Hooks.Dispatch(ctx, hook.WorkflowCancel, chain, base); err != nil {
					e.d.Logger.Warn("workflow.cancel hook failed", "job_id", job.ID, "error", err)
				}
			})
		}
	}
	e.Kick(store.Tenant(), ulid)
	return nil
}

// cancelTasks stops the daemon-pool nodes of a job that can abort. Workers see the
// flag on their next heartbeat.
func (e *Engine) cancelTasks(ctx context.Context, store repo.TenantStore, job domain.Job) {
	nodes, err := store.Jobs().ListNodes(ctx, job.ULID)
	if err != nil {
		return
	}
	for _, n := range nodes {
		if n.Status == domain.NodeRunning && n.WorkerID == daemonConsumer {
			if c, ok := e.running.Load(n.TaskID); ok {
				if d, known := e.descriptor(n.NodeType, n.TypeVersion); known && d.SupportsAbort {
					c.(context.CancelFunc)()
				}
			}
		}
	}
}

// Retry implements service.Jobs: a failed job resumes from its checkpoints.
func (j jobsService) Retry(ctx context.Context, s service.Scope, ref id.Ref, req api.JobActionRequest) (domain.Job, error) {
	store, err := j.e.tenantStore(ctx, s)
	if err != nil {
		return domain.Job{}, err
	}
	job, err := store.Jobs().Get(ctx, ref)
	if err != nil {
		return domain.Job{}, err
	}
	if job.Status != domain.JobFailed {
		return domain.Job{}, errs.Newf(errs.CodeFailedPrecondition, "job %s is %s; only a failed job can be retried", job.ID, job.Status)
	}
	if err := j.e.retryJob(ctx, store, job.ULID, req.Reason); err != nil {
		return domain.Job{}, err
	}
	return store.Jobs().GetByULID(ctx, job.ULID)
}

func (e *Engine) retryJob(ctx context.Context, store repo.TenantStore, ulid id.ULID, reason string) error {
	unlock := e.locks.lock(ulid)
	defer unlock()
	job, err := store.Jobs().GetByULID(ctx, ulid)
	if err != nil {
		return err
	}
	if job.Status != domain.JobFailed {
		return nil
	}
	rt, err := e.runtime(job)
	if err != nil {
		return err
	}
	nodes, err := store.Jobs().ListNodes(ctx, ulid)
	if err != nil {
		return err
	}
	job.Status = domain.JobQueued
	job.StatusReason = reason
	job.Error = nil
	job.FinishedAt = nil
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		for _, n := range nodes {
			spec := rt.specs[n.NodeID]
			switch {
			case n.Status == domain.NodeFailed && spec != nil && !spec.ContinueOnFailure, n.Status == domain.NodeCancelled:
				n.Status = domain.NodePending
				n.Error = nil
				n.FinishedAt = nil
				n.LeaseExpiresAt = nil
			case n.Status == domain.NodeCompleted && n.Error != nil:
				n.Error = nil
			default:
				continue
			}
			if err := tx.Jobs().PutNode(ctx, n); err != nil {
				return err
			}
		}
		if err := tx.Jobs().Update(ctx, job); err != nil {
			return err
		}
		if s := rt.def.Settings; s != nil && s.AssetStatusOnFailure != "" && job.AssetID != "" {
			a, err := tx.Assets().Get(ctx, id.Ref(job.AssetID))
			if err != nil && !errs.IsCode(err, errs.CodeNotFound) {
				return err
			}
			if err == nil && string(a.Status) == s.AssetStatusOnFailure && a.Status.CanTransition(domain.AssetIngesting) {
				a.Status = domain.AssetIngesting
				if err := tx.Assets().Update(ctx, a); err != nil {
					return err
				}
			}
		}
		data, _ := json.Marshal(map[string]any{"status": job.Status})
		_, err := tx.Jobs().AppendEvent(ctx, domain.JobEvent{JobID: job.ID, At: e.now(), Kind: domain.JobEventState, Message: "retried", Data: data})
		return err
	})
	if err != nil {
		return err
	}
	e.notifyJob(ulid)
	e.Kick(store.Tenant(), ulid)
	if p := job.Trigger.ParentJobID; p != "" {
		e.Kick(store.Tenant(), id.ULID(p))
	}
	return nil
}

// Pause implements service.Jobs (R22): what started finishes, and nothing new starts.
func (j jobsService) Pause(ctx context.Context, s service.Scope, ref id.Ref, req api.JobActionRequest) (domain.Job, error) {
	store, err := j.e.tenantStore(ctx, s)
	if err != nil {
		return domain.Job{}, err
	}
	job, err := store.Jobs().Get(ctx, ref)
	if err != nil {
		return domain.Job{}, err
	}
	if err := j.e.setPaused(ctx, store, job.ULID, req.Reason, true); err != nil {
		return domain.Job{}, err
	}
	return store.Jobs().GetByULID(ctx, job.ULID)
}

func (e *Engine) setPaused(ctx context.Context, store repo.TenantStore, ulid id.ULID, reason string, top bool) error {
	unlock := e.locks.lock(ulid)
	job, err := store.Jobs().GetByULID(ctx, ulid)
	if err != nil {
		unlock()
		return err
	}
	if job.Status == domain.JobPaused {
		unlock()
		return nil
	}
	if !job.Status.CanTransition(domain.JobPaused) {
		unlock()
		if top {
			return errs.Newf(errs.CodeFailedPrecondition, "job %s is %s and cannot be paused", job.ID, job.Status)
		}
		return nil
	}
	job.Status = domain.JobPaused
	job.StatusReason = reason
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Jobs().Update(ctx, job); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]any{"status": job.Status})
		_, err := tx.Jobs().AppendEvent(ctx, domain.JobEvent{JobID: job.ID, At: e.now(), Kind: domain.JobEventState, Message: reason, Data: data})
		return err
	})
	unlock()
	if err != nil {
		return err
	}
	e.notifyJob(ulid)
	return e.forChildren(ctx, store, ulid, func(child domain.Job) error {
		return e.setPaused(ctx, store, child.ULID, "the parent job is paused", false)
	})
}

func (e *Engine) forChildren(ctx context.Context, store repo.TenantStore, parent id.ULID, fn func(domain.Job) error) error {
	cursor := ""
	for {
		page, next, err := store.Jobs().List(ctx, repo.JobFilter{ParentJob: id.Ref(parent)}, repo.Page{Limit: 200, Cursor: cursor})
		if err != nil {
			return err
		}
		for _, c := range page {
			if !c.Status.Terminal() {
				if err := fn(c); err != nil {
					return err
				}
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// Resume implements service.Jobs. A job paused by the budget is paused again by the
// next paid node if the budget is still exceeded (I33).
func (j jobsService) Resume(ctx context.Context, s service.Scope, ref id.Ref, req api.JobActionRequest) (domain.Job, error) {
	store, err := j.e.tenantStore(ctx, s)
	if err != nil {
		return domain.Job{}, err
	}
	job, err := store.Jobs().Get(ctx, ref)
	if err != nil {
		return domain.Job{}, err
	}
	if job.Status != domain.JobPaused {
		if job.Status == domain.JobRunning || job.Status == domain.JobQueued || job.Status == domain.JobWaitingApproval || job.Status == domain.JobRetrying {
			return job, nil
		}
		return domain.Job{}, errs.Newf(errs.CodeFailedPrecondition, "job %s is %s and cannot be resumed", job.ID, job.Status)
	}
	if err := j.e.resumeJob(ctx, store, job.ULID); err != nil {
		return domain.Job{}, err
	}
	return store.Jobs().GetByULID(ctx, job.ULID)
}

func (e *Engine) resumeJob(ctx context.Context, store repo.TenantStore, ulid id.ULID) error {
	unlock := e.locks.lock(ulid)
	job, err := store.Jobs().GetByULID(ctx, ulid)
	if err != nil {
		unlock()
		return err
	}
	if job.Status != domain.JobPaused {
		unlock()
		return nil
	}
	if job.StartedAt == nil {
		job.Status = domain.JobQueued
	} else {
		job.Status = domain.JobRunning
	}
	job.StatusReason = ""
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Jobs().Update(ctx, job); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]any{"status": job.Status})
		_, err := tx.Jobs().AppendEvent(ctx, domain.JobEvent{JobID: job.ID, At: e.now(), Kind: domain.JobEventState, Message: "resumed", Data: data})
		return err
	})
	unlock()
	if err != nil {
		return err
	}
	// A pointer read while paused was answered 410 and is gone.
	if err := e.reconcileJob(ctx, store, job, false); err != nil {
		e.d.Logger.Warn("cannot re-enqueue after resume", "job_id", job.ID, "error", err)
	}
	e.notifyJob(ulid)
	e.Kick(store.Tenant(), ulid)
	return e.forChildren(ctx, store, ulid, func(child domain.Job) error {
		if child.Status != domain.JobPaused {
			return nil
		}
		return e.resumeJob(ctx, store, child.ULID)
	})
}

func isPublicationNode(nodeType string) bool {
	return strings.HasPrefix(nodeType, "publish.") || nodeType == "catalog.public.sync"
}

// Resolve implements service.Jobs: the only way out of needs_attention (§11.3, D11,
// I150).
func (j jobsService) Resolve(ctx context.Context, s service.Scope, ref id.Ref, req api.ResolveJobRequest) (domain.Job, error) {
	e := j.e
	store, err := e.tenantStore(ctx, s)
	if err != nil {
		return domain.Job{}, err
	}
	found, err := store.Jobs().Get(ctx, ref)
	if err != nil {
		return domain.Job{}, err
	}
	unlock := e.locks.lock(found.ULID)
	defer unlock()
	job, err := store.Jobs().GetByULID(ctx, found.ULID)
	if err != nil {
		return domain.Job{}, err
	}
	if job.Status != domain.JobNeedsAttention || job.Attention == nil {
		return domain.Job{}, errs.Newf(errs.CodeFailedPrecondition, "job %s is %s, not needs_attention", job.ID, job.Status)
	}
	perm := domain.PermJobResolve
	if isPublicationNode(job.Attention.NodeType) {
		perm = domain.PermJobResolvePublication
	}
	if !s.Principal.Can(perm) {
		return domain.Job{}, errs.Forbidden(string(perm))
	}
	nodeID := req.NodeID
	switch req.Action {
	case domain.ResolveRetryNode, domain.ResolveSkipNode:
		if nodeID != job.Attention.NodeID {
			return domain.Job{}, errs.Newf(errs.CodeInvalidArgument, "%s must name node %s, the one needing attention", req.Action, job.Attention.NodeID)
		}
	case domain.ResolveFail:
		nodeID = job.Attention.NodeID
	default:
		return domain.Job{}, errs.Newf(errs.CodeInvalidArgument, "unknown action %q", req.Action)
	}
	rt, err := e.runtime(job)
	if err != nil {
		return domain.Job{}, err
	}
	r := &jobRun{e: e, ctx: ctx, store: store, job: job, rt: rt, decoded: map[string]any{}, prev: job.Status}
	if err := r.loadNodes(); err != nil {
		return domain.Job{}, err
	}
	n := r.nodes[nodeID]
	now := e.now()
	details, _ := json.Marshal(map[string]any{"action": req.Action, "node_id": nodeID, "node_type": n.NodeType, "note": req.Note})
	audit := domain.AuditRecord{
		ULID: id.New(), TenantID: store.Tenant(), At: now, Actor: s.Principal.Actor(), Method: s.Principal.Method,
		Action: "job.resolve", Permission: perm, Resource: string(job.ID), Result: "allowed", RequestID: s.RequestID, Details: details,
	}
	if req.Action == domain.ResolveFail {
		jobErr := domain.JobError{Code: "resolved_as_failed", Message: "a person resolved this job as failed: " + n.Error.Message, NodeID: nodeID}
		n.Error = &jobErr
		if err := store.InTx(ctx, func(tx repo.TenantRepos) error {
			if err := tx.Jobs().PutNode(ctx, n); err != nil {
				return err
			}
			_, err := tx.Audit().Append(ctx, audit)
			return err
		}); err != nil {
			return domain.Job{}, err
		}
		r.nodes[nodeID] = n
		if err := r.finalize(domain.JobFailed, &jobErr); err != nil {
			return domain.Job{}, err
		}
		return store.Jobs().GetByULID(ctx, job.ULID)
	}
	if req.Action == domain.ResolveRetryNode {
		n.Status = domain.NodeReady
	} else {
		n.Status = domain.NodeSkipped
		n.FinishedAt = &now
	}
	n.Error = nil
	n.LeaseExpiresAt = nil
	job.Status = domain.JobRunning
	job.Attention = nil
	job.Error = nil
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Jobs().PutNode(ctx, n); err != nil {
			return err
		}
		if err := tx.Jobs().Update(ctx, job); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]any{"status": job.Status, "action": req.Action})
		if _, err := tx.Jobs().AppendEvent(ctx, domain.JobEvent{JobID: job.ID, At: now, Kind: domain.JobEventState, NodeID: nodeID, Message: req.Note, Data: data}); err != nil {
			return err
		}
		_, err := tx.Audit().Append(ctx, audit)
		return err
	})
	if err != nil {
		return domain.Job{}, err
	}
	// Queued nodes held during the wait may have lost their pointers.
	if err := e.reenqueue(ctx, store, job, r.nodes); err != nil {
		e.d.Logger.Warn("cannot re-enqueue after resolve", "job_id", job.ID, "error", err)
	}
	e.notifyJob(job.ULID)
	e.Kick(store.Tenant(), job.ULID)
	return store.Jobs().GetByULID(ctx, job.ULID)
}

func (e *Engine) reenqueue(ctx context.Context, store repo.TenantStore, job domain.Job, nodes map[string]domain.NodeExecution) error {
	var first error
	for _, n := range nodes {
		if n.Status != domain.NodeQueued {
			continue
		}
		known, err := e.d.Queue.Inflight(ctx, n.TaskID)
		if err == nil && !known {
			err = e.enqueue(ctx, store.Tenant(), job, n)
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Logs implements service.Jobs. The cursor is the last sequence read.
func (j jobsService) Logs(ctx context.Context, s service.Scope, ref id.Ref, q api.PageQuery) (api.List[domain.JobEvent], error) {
	store, err := j.e.tenantStore(ctx, s)
	if err != nil {
		return api.List[domain.JobEvent]{}, err
	}
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var after int64
	if q.Cursor != "" {
		after, err = strconv.ParseInt(q.Cursor, 10, 64)
		if err != nil {
			return api.List[domain.JobEvent]{}, errs.New(errs.CodeInvalidArgument, "invalid cursor")
		}
	}
	var out []domain.JobEvent
	for len(out) < limit {
		page, err := store.Jobs().ListEvents(ctx, ref, after, 500)
		if err != nil {
			return api.List[domain.JobEvent]{}, err
		}
		if len(page) == 0 {
			after = 0
			break
		}
		for _, ev := range page {
			after = ev.Seq
			if ev.Kind == domain.JobEventLog {
				out = append(out, ev)
				if len(out) == limit {
					break
				}
			}
		}
		if len(page) < 500 {
			if len(out) < limit {
				after = 0
			}
			break
		}
	}
	next := ""
	if after > 0 && len(out) == limit {
		next = strconv.FormatInt(after, 10)
	}
	return api.List[domain.JobEvent]{Items: nonNilList(out), NextCursor: next}, nil
}

// notifyJob wakes stream readers.
func (e *Engine) notifyJob(ulid id.ULID) {
	e.streamMu.Lock()
	defer e.streamMu.Unlock()
	for ch := range e.streams[ulid] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Subscribe implements service.Jobs. Reading a stream changes nothing, and dropping it
// does not touch the job (§5.5). It closes when the job is terminal and drained.
func (j jobsService) Subscribe(ctx context.Context, s service.Scope, ref id.Ref, afterSeq int64) (<-chan domain.JobEvent, error) {
	e := j.e
	store, err := e.tenantStore(ctx, s)
	if err != nil {
		return nil, err
	}
	job, err := store.Jobs().Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	notify := make(chan struct{}, 1)
	e.streamMu.Lock()
	if e.streams[job.ULID] == nil {
		e.streams[job.ULID] = map[chan struct{}]bool{}
	}
	e.streams[job.ULID][notify] = true
	e.streamMu.Unlock()
	out := make(chan domain.JobEvent, 32)
	go func() {
		defer close(out)
		defer func() {
			e.streamMu.Lock()
			delete(e.streams[job.ULID], notify)
			if len(e.streams[job.ULID]) == 0 {
				delete(e.streams, job.ULID)
			}
			e.streamMu.Unlock()
		}()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		after := afterSeq
		for {
			// Read the state before the events: a terminal state seen here means every
			// event of it is already readable.
			current, err := store.Jobs().GetByULID(ctx, job.ULID)
			if err != nil {
				return
			}
			page, err := store.Jobs().ListEvents(ctx, id.Ref(job.ULID), after, 200)
			if err != nil {
				return
			}
			for _, ev := range page {
				select {
				case out <- ev:
					after = ev.Seq
				case <-ctx.Done():
					return
				}
			}
			if len(page) == 200 {
				continue
			}
			if current.Status.Terminal() {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-notify:
			case <-ticker.C:
			}
		}
	}()
	return out, nil
}

// ---- service.Approvals ----

type approvalsService struct{ e *Engine }

var _ service.Approvals = approvalsService{}

// List implements service.Approvals.
func (a approvalsService) List(ctx context.Context, s service.Scope, q api.ApprovalQuery) (api.List[domain.Approval], error) {
	store, err := a.e.tenantStore(ctx, s)
	if err != nil {
		return api.List[domain.Approval]{}, err
	}
	items, next, err := store.Approvals().List(ctx, repo.ApprovalFilter{Status: q.Status, Kind: q.Kind, AssetID: id.Ref(q.AssetID)}, repo.Page{Limit: q.Limit, Cursor: q.Cursor})
	if err != nil {
		return api.List[domain.Approval]{}, err
	}
	return api.List[domain.Approval]{Items: nonNilList(items), NextCursor: next}, nil
}

// Get implements service.Approvals.
func (a approvalsService) Get(ctx context.Context, s service.Scope, ref id.Ref) (domain.Approval, error) {
	store, err := a.e.tenantStore(ctx, s)
	if err != nil {
		return domain.Approval{}, err
	}
	return store.Approvals().Get(ctx, ref)
}

// approveSegments decides what a clip_selection approval approves (§3.2, D51, I158).
func approveSegments(rec domain.Approval, req api.ApproveRequest) ([]domain.Segment, error) {
	var evidence struct {
		Ambiguous bool `json:"ambiguous"`
	}
	if len(rec.Evidence) > 0 {
		_ = json.Unmarshal(rec.Evidence, &evidence)
	}
	segments := rec.Segments
	chosen := false
	if rec.AllowAdjust {
		switch {
		case len(req.Segments) > 0:
			segments = req.Segments
			chosen = true
		case req.StartSeconds != nil && req.EndSeconds != nil:
			one := domain.Segment{StartSeconds: *req.StartSeconds, EndSeconds: *req.EndSeconds}
			if len(rec.Segments) > 0 {
				one.SourceAssetID, one.SourceVersion = rec.Segments[0].SourceAssetID, rec.Segments[0].SourceVersion
			}
			segments = []domain.Segment{one}
			chosen = true
		case req.StartSeconds != nil || req.EndSeconds != nil:
			return nil, errs.New(errs.CodeInvalidArgument, "adjusting a selection needs both start and end")
		}
	}
	if rec.Kind != domain.ApprovalClipSelection {
		return segments, nil
	}
	if evidence.Ambiguous && !chosen {
		return nil, errs.Newf(errs.CodeFailedPrecondition, "approval %s is ambiguous; choose the start and end, the selection is not cut at random (§3.2)", rec.ID)
	}
	if len(segments) == 0 {
		return nil, errs.Newf(errs.CodeFailedPrecondition, "approval %s has no selection to approve; give start and end", rec.ID)
	}
	out := make([]domain.Segment, len(segments))
	for i, seg := range segments {
		if seg.StartSeconds < 0 || seg.EndSeconds <= seg.StartSeconds {
			return nil, errs.New(errs.CodeInvalidArgument, "a segment must have 0 <= start < end")
		}
		if seg.SourceAssetID == "" {
			seg.SourceAssetID = rec.AssetID
		}
		if seg.SourceVersion == "" {
			seg.SourceVersion = rec.AssetVersion
		}
		out[i] = seg
	}
	return out, nil
}

// Approve implements service.Approvals with D7's rule.
func (a approvalsService) Approve(ctx context.Context, s service.Scope, ref id.Ref, req api.ApproveRequest) (domain.Approval, error) {
	store, err := a.e.tenantStore(ctx, s)
	if err != nil {
		return domain.Approval{}, err
	}
	rec, err := store.Approvals().Get(ctx, ref)
	if err != nil {
		return domain.Approval{}, err
	}
	if !domain.MayDecide(s.Principal, rec.Kind, rec.Approvers) {
		return domain.Approval{}, errs.Forbidden(string(rec.Kind.RequiredPermission()))
	}
	if rec.Status != domain.ApprovalPending {
		return domain.Approval{}, errs.Newf(errs.CodeFailedPrecondition, "approval %s is already %s", rec.ID, rec.Status)
	}
	segments, err := approveSegments(rec, req)
	if err != nil {
		return domain.Approval{}, err
	}
	return a.decide(ctx, s, store, rec, repo.ApprovalDecision{
		Status: domain.ApprovalApproved, DecidedBy: s.Principal.Actor(), DecidedAt: a.e.now(), Reason: req.Note, Segments: segments,
	})
}

// Reject implements service.Approvals with D7's rule.
func (a approvalsService) Reject(ctx context.Context, s service.Scope, ref id.Ref, req api.RejectRequest) (domain.Approval, error) {
	store, err := a.e.tenantStore(ctx, s)
	if err != nil {
		return domain.Approval{}, err
	}
	rec, err := store.Approvals().Get(ctx, ref)
	if err != nil {
		return domain.Approval{}, err
	}
	if !domain.MayDecide(s.Principal, rec.Kind, rec.Approvers) {
		return domain.Approval{}, errs.Forbidden(string(rec.Kind.RequiredPermission()))
	}
	if rec.Status != domain.ApprovalPending {
		return domain.Approval{}, errs.Newf(errs.CodeFailedPrecondition, "approval %s is already %s", rec.ID, rec.Status)
	}
	return a.decide(ctx, s, store, rec, repo.ApprovalDecision{
		Status: domain.ApprovalRejected, DecidedBy: s.Principal.Actor(), DecidedAt: a.e.now(), Reason: req.Reason,
	})
}

// decide commits the decision, its event and its audit together, then wakes the job.
func (a approvalsService) decide(ctx context.Context, s service.Scope, store repo.TenantStore, rec domain.Approval, d repo.ApprovalDecision) (domain.Approval, error) {
	e := a.e
	job, err := store.Jobs().Get(ctx, id.Ref(rec.JobID))
	if err != nil {
		return domain.Approval{}, err
	}
	var out domain.Approval
	details, _ := json.Marshal(map[string]any{"kind": rec.Kind, "decision": d.Status, "asset_id": rec.AssetID, "job_id": rec.JobID})
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		decided, err := tx.Approvals().Decide(ctx, id.Ref(rec.ID), d)
		if err != nil {
			return err
		}
		out = decided
		if _, err := events.Append(ctx, tx, store.Tenant(), events.JobSource(job), event.Draft{
			Type: event.ApprovalDecided, AssetID: string(rec.AssetID),
			Payload: map[string]any{"approval_id": rec.ID, "kind": rec.Kind, "decision": d.Status, "decided_by": d.DecidedBy},
		}, e.now()); err != nil {
			return err
		}
		_, err = tx.Audit().Append(ctx, domain.AuditRecord{
			ULID: id.New(), TenantID: store.Tenant(), At: d.DecidedAt, Actor: d.DecidedBy, Method: s.Principal.Method,
			Action: "approval." + string(d.Status), Permission: rec.Kind.RequiredPermission(), Resource: string(rec.ID),
			Result: "allowed", RequestID: s.RequestID, Details: details,
		})
		return err
	})
	if err != nil {
		return domain.Approval{}, err
	}
	e.d.Bus.Notify()
	e.Kick(store.Tenant(), job.ULID)
	return out, nil
}

// ---- service.Introspection ----

type introspectionService struct{ e *Engine }

var _ service.Introspection = introspectionService{}

// Nodes implements service.Introspection: what this daemon registered, plus the
// catalog's worker-pool nodes, which run elsewhere.
func (i introspectionService) Nodes(context.Context, service.Scope) ([]node.Descriptor, error) {
	seen := map[string]bool{}
	var out []node.Descriptor
	if i.e.d.Nodes != nil {
		for _, d := range i.e.d.Nodes.List() {
			seen[d.Type+"@"+strconv.Itoa(d.TypeVersion)] = true
			out = append(out, d)
		}
	}
	cat, err := schemas.NodeCatalog()
	if err != nil {
		return nil, err
	}
	for _, c := range cat.Nodes {
		if !seen[c.Type+"@"+strconv.Itoa(c.TypeVersion)] {
			out = append(out, c.Descriptor())
		}
	}
	return out, nil
}

// Node implements service.Introspection.
func (i introspectionService) Node(ctx context.Context, s service.Scope, nodeType string) (node.Descriptor, error) {
	version := latestTypeVersion(i.e, nodeType)
	if version == 0 {
		return node.Descriptor{}, errs.NotFound("node type")
	}
	d, _ := i.e.descriptor(nodeType, version)
	return d, nil
}

type hookEntry struct {
	info  api.HookInfo
	decl  wfdef.HookDecl
	point hook.Point
}

var workflowPoints = []hook.Point{hook.WorkflowBefore, hook.WorkflowStart, hook.WorkflowAfter, hook.WorkflowFinish, hook.WorkflowError, hook.WorkflowCancel}
var nodePoints = []hook.Point{hook.NodeBefore, hook.NodeAfter, hook.NodeError, hook.NodeFinish}

func listHooks(set *wfdef.HookSet, points []hook.Point, level string) []hookEntry {
	var out []hookEntry
	for _, p := range points {
		for _, d := range hooks.Select(set, p) {
			out = append(out, hookEntry{
				info:  api.HookInfo{ID: d.ID, Point: string(p), Level: level, Plugin: d.Plugin, Action: d.Action},
				decl:  d,
				point: p,
			})
		}
	}
	return out
}

func (i introspectionService) hookEntries(ctx context.Context, s service.Scope, workflowID string) ([]hookEntry, error) {
	var out []hookEntry
	if i.e.d.Config != nil {
		cfg, err := i.e.d.Config.TenantConfig(ctx, s.Tenant)
		if err != nil && !errs.IsCode(err, errs.CodeNotFound) {
			return nil, err
		}
		if err == nil {
			out = append(out, listHooks(&cfg.Hooks.Workflow, workflowPoints, "global")...)
			out = append(out, listHooks(&cfg.Hooks.Node, nodePoints, "global")...)
		}
	}
	var defs []*workflow.Entry
	if workflowID != "" {
		entry, ok := i.e.d.Workflows.Get(workflowID, "")
		if !ok {
			return nil, errs.NotFound("workflow")
		}
		defs = []*workflow.Entry{entry}
	} else {
		defs = i.e.d.Workflows.List()
	}
	for _, entry := range defs {
		out = append(out, listHooks(entry.Definition.Hooks, workflowPoints, "workflow")...)
		for k := range entry.Definition.Nodes {
			out = append(out, listHooks(entry.Definition.Nodes[k].Hooks, nodePoints, "node")...)
		}
	}
	return out, nil
}

// Hooks implements service.Introspection.
func (i introspectionService) Hooks(ctx context.Context, s service.Scope, workflowID string) ([]api.HookInfo, error) {
	entries, err := i.hookEntries(ctx, s, workflowID)
	if err != nil {
		return nil, err
	}
	out := make([]api.HookInfo, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.info)
	}
	return out, nil
}

// TestHook implements service.Introspection: one hook on demand, with its own policy
// off, so a person sees what it says.
func (i introspectionService) TestHook(ctx context.Context, s service.Scope, hookID string, req api.TestHookRequest) (map[string]any, error) {
	entries, err := i.hookEntries(ctx, s, "")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.decl.ID != hookID {
			continue
		}
		decl := entry.decl
		decl.OnFailure = wfdef.HookAbort
		res, err := i.e.d.Hooks.Dispatch(ctx, entry.point, hook.Chain{Global: []wfdef.HookDecl{decl}}, hook.Invocation{TenantID: s.Tenant, AssetID: req.AssetID, TraceID: s.TraceID})
		out := map[string]any{"hook": hookID, "point": entry.point, "allow": res.Allow, "reason": res.Reason}
		if err != nil {
			out["allow"] = false
			out["reason"] = errs.Public(err).Message
			if b, ok := hooks.AsBlocked(err); ok {
				out["reason"] = b.Reason
			}
		}
		return out, nil
	}
	return nil, errs.NotFound("hook")
}
