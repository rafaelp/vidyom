package jobs

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/internal/events"
	"github.com/rafaelp/vidyom/internal/workflow"
	controlnodes "github.com/rafaelp/vidyom/nodes/control"
)

// nodeEnv implements node.DaemonEnv for pools engine and daemon, so all of it is
// available to nodes running in the daemon (plan section 10).
type nodeEnv struct {
	e        *Engine
	store    repo.TenantStore
	job      domain.Job
	nodeID   string
	taskID   string
	leaseID  string
	ancestor []id.ULID
}

var _ node.DaemonEnv = (*nodeEnv)(nil)

func (env *nodeEnv) Tenant() domain.TenantID { return env.store.Tenant() }
func (env *nodeEnv) JobID() id.Friendly      { return env.job.ID }
func (env *nodeEnv) JobULID() id.ULID        { return env.job.ULID }
func (env *nodeEnv) NodeID() string          { return env.nodeID }
func (env *nodeEnv) TraceID() string         { return env.job.TraceID }

func (env *nodeEnv) Logger() *slog.Logger {
	return env.e.d.Logger.With("tenant_id", env.Tenant(), "job_id", env.job.ID, "node_id", env.nodeID, "trace_id", env.job.TraceID)
}

// Progress reports through the task heartbeat path; it never fails a node.
func (env *nodeEnv) Progress(percent float64, message string) {
	if env.taskID == "" {
		return
	}
	env.e.reportProgress(env.taskID, env.leaseID, percent, message)
}

func (env *nodeEnv) Scratch() node.Scratch { return &scratchArea{env: env} }

func (env *nodeEnv) Storage(ctx context.Context, role domain.StorageRole) (connector.Storage, error) {
	if env.e.d.Storage == nil {
		return nil, errs.New(errs.CodeUnavailable, "storage is not wired in this daemon")
	}
	return env.e.d.Storage.Storage(ctx, env.Tenant(), role)
}

func (env *nodeEnv) HasPermission(p domain.Permission) bool {
	for _, have := range env.job.Permissions {
		if have == p {
			return true
		}
	}
	return false
}

// OpenInput resolves a reference to a local path. Daemon-pool nodes rarely need it:
// the media worker's version has the verified cache, while this one downloads into
// the job's scratch area, under the same rules (§5.4, §6.4).
func (env *nodeEnv) OpenInput(ctx context.Context, ref string, forbid ...domain.StorageRole) (node.LocalInput, error) {
	u, err := domain.ParseInputURI(ref)
	if err != nil {
		return node.LocalInput{}, errs.Wrap(errs.CodeInvalidArgument, err, err.Error())
	}
	if err := u.ForbidRole(forbid...); err != nil {
		return node.LocalInput{}, err
	}
	switch u.Kind {
	case domain.URIServer:
		path, err := domain.ResolveServerPath(env.e.d.Daemon.IngestRoots, u.Path)
		if err != nil {
			return node.LocalInput{}, err
		}
		return statLocal(path)
	case domain.URIUpload:
		upload, err := env.store.Uploads().Get(ctx, id.Ref(u.UploadRef))
		if err != nil {
			return node.LocalInput{}, err
		}
		if upload.Status != domain.UploadComplete {
			return node.LocalInput{}, errs.Newf(errs.CodeFailedPrecondition, "upload %s is not complete", upload.ID)
		}
		return statLocal(filepath.Join(env.e.d.Daemon.SpoolDir, string(env.Tenant()), string(upload.ULID)))
	case domain.URIScratch:
		path, err := env.Scratch().Resolve(u)
		if err != nil {
			return node.LocalInput{}, err
		}
		return statLocal(path)
	case domain.URIStorage:
		return env.download(ctx, u)
	default:
		return node.LocalInput{}, errs.NotImplemented("import: inputs")
	}
}

func statLocal(path string) (node.LocalInput, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return node.LocalInput{}, errs.NotFound("input file")
		}
		return node.LocalInput{}, errs.Wrap(errs.CodeUnavailable, err, "cannot read the input file")
	}
	return node.LocalInput{Path: path, Size: info.Size()}, nil
}

func (env *nodeEnv) download(ctx context.Context, u domain.InputURI) (node.LocalInput, error) {
	st, err := env.Storage(ctx, u.Role)
	if err != nil {
		return node.LocalInput{}, err
	}
	r, info, err := st.Get(ctx, u.Key)
	if err != nil {
		return node.LocalInput{}, err
	}
	defer r.Close()
	path, err := env.Scratch().Path(".inputs/" + strings.ReplaceAll(u.Key, "/", "_"))
	if err != nil {
		return node.LocalInput{}, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return node.LocalInput{}, errs.Wrap(errs.CodeUnavailable, err, "cannot create a local copy")
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return node.LocalInput{}, errs.Wrap(errs.CodeUnavailable, err, "cannot download the object")
	}
	if err := f.Close(); err != nil {
		return node.LocalInput{}, errs.Wrap(errs.CodeUnavailable, err, "cannot write the local copy")
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return node.LocalInput{}, errs.Wrap(errs.CodeUnavailable, err, "cannot write the local copy")
	}
	return node.LocalInput{Path: path, Role: u.Role, SHA256: info.SHA256, Size: info.SizeBytes}, nil
}

// Store implements node.DaemonEnv.
func (env *nodeEnv) Store() repo.TenantStore { return env.store }

// Emit implements node.DaemonEnv. It fills the job context, and continues the job's
// event chain (§8.2). A nil tx commits by itself.
func (env *nodeEnv) Emit(ctx context.Context, tx repo.TenantRepos, d event.Draft) error {
	src := events.JobSource(env.job)
	if tx != nil {
		_, err := events.Append(ctx, tx, env.Tenant(), src, d, env.e.now())
		return err
	}
	err := env.store.InTx(ctx, func(tx repo.TenantRepos) error {
		_, err := events.Append(ctx, tx, env.Tenant(), src, d, env.e.now())
		return err
	})
	if err == nil {
		env.e.d.Bus.Notify()
	}
	return err
}

// Publishing implements node.DaemonEnv.
func (env *nodeEnv) Publishing(ctx context.Context, destination string) (connector.Publishing, error) {
	if env.e.d.Publishing == nil {
		return nil, errs.New(errs.CodeUnavailable, "publishing is not wired in this daemon")
	}
	return env.e.d.Publishing.Publishing(ctx, env.Tenant(), destination)
}

// TenantConfig implements node.DaemonEnv.
func (env *nodeEnv) TenantConfig(ctx context.Context) (config.Tenant, error) {
	if env.e.d.Config == nil {
		return config.Tenant{}, errs.New(errs.CodeUnavailable, "tenant configuration is not wired in this daemon")
	}
	return env.e.d.Config.TenantConfig(ctx, env.Tenant())
}

// Embedder implements node.DaemonEnv.
func (env *nodeEnv) Embedder(ctx context.Context) (connector.Embedder, error) {
	if env.e.d.Connectors == nil {
		return nil, errs.New(errs.CodeUnavailable, "connectors are not wired in this daemon")
	}
	cfg, err := env.TenantConfig(ctx)
	if err != nil {
		return nil, err
	}
	factory, err := env.e.d.Connectors.Embedder(env.e.d.EmbedderDriver)
	if err != nil {
		return nil, err
	}
	return factory(ctx, connector.AIParams{Tenant: env.Tenant(), AI: cfg.AI, Daemon: env.e.d.Daemon.AIWorker, Secrets: env.Secrets()})
}

// LLM implements node.DaemonEnv. Every path to an LLM goes through
// config.AllowedEndpoints with the asset's effective policy (D8). None allowed is
// failed_precondition, which the AI nodes turn into a result marked unavailable (I28).
func (env *nodeEnv) LLM(ctx context.Context, asset domain.Asset) (connector.LLM, error) {
	if env.e.d.Connectors == nil {
		return nil, errs.New(errs.CodeUnavailable, "connectors are not wired in this daemon")
	}
	cfg, err := env.TenantConfig(ctx)
	if err != nil {
		return nil, err
	}
	processing := asset.AIProcessing
	if processing == "" {
		processing = domain.AILocalOnly
	}
	allowed := config.AllowedEndpoints(cfg.AI.LLMEndpoints, processing)
	if len(allowed) == 0 {
		return nil, errs.Newf(errs.CodeFailedPrecondition, "no LLM endpoint is allowed for asset %s under %s (D8)", asset.ID, processing)
	}
	factory, err := env.e.d.Connectors.LLM(env.e.d.LLMDriver)
	if err != nil {
		return nil, err
	}
	return factory(ctx, allowed[0], env.Secrets())
}

// Secrets implements node.DaemonEnv.
func (env *nodeEnv) Secrets() connector.SecretResolver {
	if env.e.d.SecretsFor == nil {
		return noSecrets{}
	}
	return env.e.d.SecretsFor(env.Tenant())
}

// DaemonConfig gives catalog nodes the host's settings (catalognodes.HostEnv).
func (env *nodeEnv) DaemonConfig() config.Daemon { return env.e.d.Daemon }

type noSecrets struct{}

func (noSecrets) ResolveSecret(context.Context, string) ([]byte, error) {
	return nil, errs.New(errs.CodeUnavailable, "the vault is not wired in this daemon")
}

// ---- scratch ----

var scratchFile = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

type scratchArea struct{ env *nodeEnv }

func (s *scratchArea) dir(job id.ULID, nodeID string) (string, error) {
	root := s.env.e.d.Daemon.ScratchDir
	if root == "" {
		return "", errs.New(errs.CodeUnavailable, "no scratch directory is configured")
	}
	return filepath.Join(root, string(s.env.Tenant()), string(job), nodeID), nil
}

func (s *scratchArea) Path(name string) (string, error) {
	if !scratchFile.MatchString(name) || strings.Contains(name, "..") {
		return "", errs.Newf(errs.CodeInvalidArgument, "invalid scratch file name %q", name)
	}
	dir, err := s.dir(s.env.job.ULID, s.env.nodeID)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", errs.Wrap(errs.CodeUnavailable, err, "cannot create the scratch directory")
	}
	return path, nil
}

func (s *scratchArea) URI(name string) string {
	return (domain.InputURI{Kind: domain.URIScratch, JobULID: string(s.env.job.ULID), NodeID: s.env.nodeID, File: name}).String()
}

// Resolve refuses other jobs' files, except the ancestors a child may read (I182).
// The tenant never comes from the URI.
func (s *scratchArea) Resolve(u domain.InputURI) (string, error) {
	if u.Kind != domain.URIScratch {
		return "", errs.New(errs.CodeInvalidArgument, "not a scratch reference")
	}
	job, err := id.ParseULID(u.JobULID)
	if err != nil {
		return "", errs.New(errs.CodeInvalidArgument, "invalid scratch reference")
	}
	allowed := job == s.env.job.ULID
	for _, a := range s.env.ancestor {
		allowed = allowed || a == job
	}
	if !allowed {
		return "", errs.New(errs.CodeForbidden, "that scratch file belongs to another job")
	}
	dir, err := s.dir(job, u.NodeID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.FromSlash(u.File)), nil
}

// ---- pool engine ----

// controlEnv adds what control nodes need, for the node running under this look at
// the job (controlnodes.Env).
type controlEnv struct {
	*nodeEnv
	run *jobRun
}

var _ controlnodes.Env = controlEnv{}

func (r *jobRun) controlEnv(nodeID string) controlEnv {
	return controlEnv{nodeEnv: &nodeEnv{e: r.e, store: r.store, job: r.job, nodeID: nodeID}, run: r}
}

// EvalCondition implements controlnodes.Env.
func (c controlEnv) EvalCondition(ctx context.Context, expr string) (bool, error) {
	cond, err := workflow.CompileCondition(expr)
	if err != nil {
		return false, err
	}
	return cond.Eval(ctx, c.run.scopeFor(cond.Nodes))
}

// MergeInputs implements controlnodes.Env.
func (c controlEnv) MergeInputs() []string {
	var out []string
	for _, pred := range c.run.rt.graph.Preds[c.nodeID] {
		if c.run.inputState(pred) == inputDone {
			out = append(out, pred)
		}
	}
	sort.Strings(out)
	return out
}

// childKey is the idempotency key of a node's child: child:<parent>:<node>:<key>:g<n>.
// A new generation exists only to replace a child cancelled by an earlier attempt,
// because a cancelled job is final.
func childKey(parent id.ULID, nodeID, key string, generation int) string {
	return "child:" + string(parent) + ":" + nodeID + ":" + key + ":g" + strconv.Itoa(generation)
}

func parseChildKey(k string) (key string, generation int, ok bool) {
	parts := strings.Split(k, ":")
	if len(parts) != 5 || parts[0] != "child" || !strings.HasPrefix(parts[4], "g") {
		return "", 0, false
	}
	n, err := strconv.Atoi(parts[4][1:])
	if err != nil {
		return "", 0, false
	}
	return parts[3], n, true
}

// childJobs lists a node's child jobs, every generation.
func (r *jobRun) childJobs(nodeID string) ([]domain.Job, error) {
	var out []domain.Job
	cursor := ""
	for {
		page, next, err := r.store.Jobs().List(r.ctx, repo.JobFilter{ParentJob: id.Ref(r.job.ULID)}, repo.Page{Limit: 200, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, j := range page {
			if j.Trigger.ParentNode == nodeID {
				out = append(out, j)
			}
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// latestChildren keeps the newest generation per key.
func latestChildren(jobs []domain.Job) map[string]domain.Job {
	type best struct {
		job domain.Job
		gen int
	}
	seen := map[string]best{}
	for _, j := range jobs {
		key, gen, ok := parseChildKey(j.IdempotencyKey)
		if !ok {
			continue
		}
		if cur, found := seen[key]; !found || gen >= cur.gen {
			seen[key] = best{j, gen}
		}
	}
	out := make(map[string]domain.Job, len(seen))
	for k, b := range seen {
		out[k] = b.job
	}
	return out
}

// stale reports a child that ended badly before this attempt of the node began: its
// outcome belongs to an earlier try, which job retry already answered.
func (c controlEnv) stale(j domain.Job) bool {
	if j.Status != domain.JobFailed && j.Status != domain.JobCancelled {
		return false
	}
	n := c.run.nodes[c.nodeID]
	return j.FinishedAt != nil && n.StartedAt != nil && !j.FinishedAt.After(*n.StartedAt)
}

func childState(key string, j domain.Job) controlnodes.ChildState {
	return controlnodes.ChildState{Key: key, JobID: j.ID, Status: j.Status, Outputs: j.Outputs, Error: j.Error}
}

// Children implements controlnodes.Env.
func (c controlEnv) Children(context.Context) ([]controlnodes.ChildState, error) {
	jobs, err := c.run.childJobs(c.nodeID)
	if err != nil {
		return nil, node.Transient(err)
	}
	latest := latestChildren(jobs)
	out := make([]controlnodes.ChildState, 0, len(latest))
	for key, j := range latest {
		if !c.stale(j) {
			out = append(out, childState(key, j))
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Key < out[k].Key })
	return out, nil
}

// StartChild implements controlnodes.Env. A failed child from an earlier attempt
// resumes from its checkpoints, keeping its idempotency keys (I36).
func (c controlEnv) StartChild(ctx context.Context, req controlnodes.ChildRequest) (controlnodes.ChildState, error) {
	r := c.run
	jobs, err := r.childJobs(c.nodeID)
	if err != nil {
		return controlnodes.ChildState{}, node.Transient(err)
	}
	generation := 0
	if cur, found := latestChildren(jobs)[req.Key]; found {
		_, gen, _ := parseChildKey(cur.IdempotencyKey)
		switch {
		case !c.stale(cur):
			return childState(req.Key, cur), nil
		case cur.Status == domain.JobFailed:
			if err := r.e.retryJob(ctx, r.store, cur.ULID, "the parent job was retried"); err != nil {
				return controlnodes.ChildState{}, node.Transient(err)
			}
			requeued, err := r.store.Jobs().GetByULID(ctx, cur.ULID)
			if err != nil {
				return controlnodes.ChildState{}, node.Transient(err)
			}
			return childState(req.Key, requeued), nil
		default:
			generation = gen + 1
		}
	}
	if err := r.checkDepth(); err != nil {
		return controlnodes.ChildState{}, node.Permanent(err)
	}
	child, err := r.createChild(req, childKey(r.job.ULID, c.nodeID, req.Key, generation), c.nodeID)
	if err != nil {
		return controlnodes.ChildState{}, node.Permanent(err)
	}
	return childState(req.Key, child), nil
}

func (r *jobRun) checkDepth() error {
	depth := 0
	parent := r.job.Trigger.ParentJobID
	for parent != "" && depth <= MaxSubworkflowDepth {
		depth++
		j, err := r.store.Jobs().GetByULID(r.ctx, id.ULID(parent))
		if err != nil {
			break
		}
		parent = j.Trigger.ParentJobID
	}
	if depth >= MaxSubworkflowDepth {
		return errs.Newf(errs.CodeFailedPrecondition, "subworkflows are nested deeper than %d", MaxSubworkflowDepth)
	}
	return nil
}

// createChild starts a child from this job's snapshot, never from the registry, so a
// new version never changes a job in progress (§6.5, I144).
func (r *jobRun) createChild(req controlnodes.ChildRequest, key, nodeID string) (domain.Job, error) {
	bundle := r.rt.def.Subworkflows
	raw, ok := workflow.RawFromBundle(bundle)(req.Workflow, req.Version)
	if !ok {
		return domain.Job{}, errs.Newf(errs.CodeFailedPrecondition, "workflow %s is not in this job's snapshot", workflow.ChildTarget{Workflow: req.Workflow, Version: req.Version}.BundleKey())
	}
	snapshot, err := workflow.BuildSnapshot(raw, workflow.RawFromBundle(bundle))
	if err != nil {
		return domain.Job{}, err
	}
	def, err := wfdef.Parse(snapshot)
	if err != nil {
		return domain.Job{}, err
	}
	encoded, err := json.Marshal(req.Inputs)
	if err != nil {
		return domain.Job{}, errs.Wrap(errs.CodeInvalidArgument, err, "child inputs do not encode")
	}
	inputs, err := workflow.BindInputs(def, encoded, workflow.BindOptions{})
	if err != nil {
		return domain.Job{}, err
	}
	job, _, err := r.e.createJob(r.ctx, r.store, jobSpec{
		def: def, snapshot: snapshot, inputs: inputs, key: key,
		submittedBy: r.job.SubmittedBy, permissions: r.job.Permissions, traceID: r.job.TraceID,
		trigger: domain.JobTrigger{
			Type: "subworkflow", ParentJobID: string(r.job.ULID), ParentNode: nodeID,
			Depth: r.job.Trigger.Depth, EventID: "", CausationID: r.job.Trigger.CausationID,
		},
	})
	return job, err
}
