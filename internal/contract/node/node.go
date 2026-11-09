// Package node defines workflow nodes (§6, §5.5). A node is a workflow action;
// it uses connectors for providers, and it never knows which workflow it is in.
// schemas/node-catalog.v1.json is the authoritative list of node types.
package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// SideEffect is the side-effect class every node type declares (D11).
type SideEffect string

const (
	// EffectNone is pure and retried freely.
	EffectNone SideEffect = "none"
	// EffectIdempotent is retried with its idempotency key.
	EffectIdempotent SideEffect = "idempotent"
	// EffectReconcilable must query remote state before any retry; the node also
	// implements Reconciler.
	EffectReconcilable SideEffect = "reconcilable"
	// EffectUnsafe is never retried automatically. Failure or interruption moves
	// the job to needs_attention until a person resolves it.
	EffectUnsafe SideEffect = "unsafe"
)

// Pool says where a node runs (I7).
type Pool string

const (
	PoolEngine Pool = "engine" // inline in the engine, no task
	PoolDaemon Pool = "daemon" // in vidyomd, needs the database or the vault
	PoolMedia  Pool = "media"  // vidyom-worker with FFmpeg and storage
	PoolAI     Pool = "ai"     // vidyom-worker with the Python AI worker
)

// Pools lists the pools that receive tasks through Redis.
var Pools = []Pool{PoolDaemon, PoolMedia, PoolAI}

// Requirements are what a task needs from a worker (D40).
type Requirements struct {
	CPUCores int  `json:"cpu_cores,omitempty"`
	MemoryMB int  `json:"memory_mb,omitempty"`
	DiskMB   int  `json:"disk_mb,omitempty"`
	GPU      bool `json:"gpu,omitempty"`
}

// Descriptor is a node type's contract, reported by GET /nodes.
type Descriptor struct {
	Type          string          `json:"type"`
	TypeVersion   int             `json:"typeVersion"`
	Title         string          `json:"title,omitempty"`
	Owner         string          `json:"owner,omitempty"`
	Pool          Pool            `json:"pool"`
	SideEffect    SideEffect      `json:"sideEffect"`
	SupportsAbort bool            `json:"supportsAbort,omitempty"`
	Paid          bool            `json:"paid,omitempty"` // budget-gated (D19)
	Requirements  Requirements    `json:"requirements,omitzero"`
	Parameters    json.RawMessage `json:"parameters,omitempty"` // JSON Schema
	Output        json.RawMessage `json:"output,omitempty"`     // JSON Schema
}

// Input is one execution of a node. Parameters have ${...} references resolved
// and have passed the catalog's schema.
type Input struct {
	Parameters     json.RawMessage
	Attempt        int // 1 on the first run
	IdempotencyKey string
	LeaseID        string
	// PersonRetry means a person looked at an unknown outcome and asked for this attempt
	// (§11.3, I288). A connector that cannot say what happened no longer blocks it.
	PersonRetry bool
}

// Bind decodes Parameters into v, as invalid_argument on failure.
func (in Input) Bind(v any) error {
	if err := json.Unmarshal(in.Parameters, v); err != nil {
		return errs.Wrap(errs.CodeInvalidArgument, err, "invalid node parameters")
	}
	return nil
}

// Suspension pauses a node without holding a goroutine or a CLI session (§5.5).
// The engine persists it and later runs the node's Resume with what it waited for.
type Suspension struct {
	Reason      string // "approval", "wait", "subworkflow"
	ApprovalID  id.Friendly
	ResumeAfter int64 // Unix seconds, for flow.wait; 0 when none
}

// Result is a node's outcome. Exactly one of Output or Suspend applies.
type Result struct {
	Output  json.RawMessage
	Suspend *Suspension
	Usage   []domain.UsageRecord // recorded by the daemon against the budget (D19)
	Tools   map[string]string    // tool and model versions for provenance (R21)
}

// JSONResult builds a Result from any value.
func JSONResult(v any) (Result, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return Result{}, fmt.Errorf("encode node output: %w", err)
	}
	return Result{Output: data}, nil
}

// Node is an implementation of a node type.
type Node interface {
	Descriptor() Descriptor
	// Execute runs the node. It must watch ctx: cancellation is cooperative
	// (D11). A node that cannot abort finishes, and the engine stops at the next
	// checkpoint. Errors are classified with Transient, Permanent or Unknown; an
	// unclassified error counts as Permanent.
	Execute(ctx context.Context, env Env, in Input) (Result, error)
}

// ReconcileOutcome is the answer of a Reconciler.
type ReconcileOutcome string

const (
	// ReconcileDone: the effect already happened; Output of the result applies.
	ReconcileDone ReconcileOutcome = "done"
	// ReconcileNotStarted: the effect never happened; safe to run again.
	ReconcileNotStarted ReconcileOutcome = "not_started"
	// ReconcileUnknown: cannot tell; the job moves to needs_attention.
	ReconcileUnknown ReconcileOutcome = "unknown"
)

// Reconciler is required for class reconcilable. The engine calls it before any
// retry, including after a daemon or worker restart.
type Reconciler interface {
	Reconcile(ctx context.Context, env Env, in Input) (ReconcileOutcome, Result, error)
}

// Resumer is implemented by nodes that suspend. Decision carries what the
// suspension waited for, such as the approval's outcome.
type Resumer interface {
	Resume(ctx context.Context, env Env, in Input, decision json.RawMessage) (Result, error)
}

// Failure class, attached to errors a node returns.
type failureClass int

const (
	classPermanent failureClass = iota
	classTransient
	classUnknown
)

type classified struct {
	error
	class failureClass
}

func (c classified) Unwrap() error { return c.error }

// Transient marks an error worth retrying within the node's class.
func Transient(err error) error { return classified{err, classTransient} }

// Permanent marks an error that no retry can fix.
func Permanent(err error) error { return classified{err, classPermanent} }

// Unknown marks an external effect whose outcome is uncertain, for example a
// connection lost mid-upload. An unsafe node, or a reconcilable node that cannot
// reconcile, moves the job to needs_attention.
func Unknown(err error) error { return classified{err, classUnknown} }

// IsTransient reports whether err was marked Transient.
func IsTransient(err error) bool {
	var c classified
	return errors.As(err, &c) && c.class == classTransient
}

// IsUnknownOutcome reports whether err was marked Unknown.
func IsUnknownOutcome(err error) bool {
	var c classified
	return errors.As(err, &c) && c.class == classUnknown
}

// Scratch is the job's working area on the shared volume (I5). Everything in it
// is deleted at job end.
type Scratch interface {
	// Path returns a path this node may write, creating parent directories.
	Path(name string) (string, error)
	// URI returns the scratch:// reference for a file written under Path.
	URI(name string) string
	// Resolve turns a scratch:// reference of this job into a local path, refusing
	// other tenants' and other jobs' files.
	Resolve(uri domain.InputURI) (string, error)
}

// Env is what every node gets, in the daemon and in a worker.
type Env interface {
	Tenant() domain.TenantID
	JobID() id.Friendly
	JobULID() id.ULID
	NodeID() string
	TraceID() string
	Logger() *slog.Logger
	// Progress reports percent and message; it is throttled and best-effort.
	Progress(percent float64, message string)
	Scratch() Scratch
	// Storage opens a role for this tenant.
	Storage(ctx context.Context, role domain.StorageRole) (connector.Storage, error)
	// OpenInput resolves a server:, upload:, storage:// or scratch:// reference to
	// a local readable path, downloading to the verified cache when needed. It
	// refuses roles in forbid. import: returns errs.CodeNotImplemented until an
	// import connector exists.
	OpenInput(ctx context.Context, ref string, forbid ...domain.StorageRole) (LocalInput, error)
	// HasPermission reports whether the job carries a permission granted at
	// submission, e.g. storage.archive.restore.
	HasPermission(p domain.Permission) bool
}

// LocalInput is a resolved local file. Role is set for stored objects and for
// cached copies of them, so video.cut can check it (§6.4).
type LocalInput struct {
	Path   string
	Role   domain.StorageRole
	SHA256 string
	Size   int64
}

// DaemonEnv is what pool daemon nodes get. Worker-pool nodes must not assert to
// it, so vidyom-worker stays free of database code.
type DaemonEnv interface {
	Env
	// Store is the tenant's repositories. Use InTx to commit state with its events.
	Store() repo.TenantStore
	// Emit writes an event to the outbox, in tx when tx is non-nil (§8.2).
	Emit(ctx context.Context, tx repo.TenantRepos, d event.Draft) error
	Publishing(ctx context.Context, destination string) (connector.Publishing, error)
	TenantConfig(ctx context.Context) (config.Tenant, error)
	Embedder(ctx context.Context) (connector.Embedder, error)
	// LLM returns an endpoint allowed for this asset's ai_processing, or
	// errs.CodeFailedPrecondition when none is (D8, I28).
	LLM(ctx context.Context, asset domain.Asset) (connector.LLM, error)
	// Secrets resolves a credentialsRef from the tenant vault.
	Secrets() connector.SecretResolver
}

// RequireDaemon returns the DaemonEnv or a clear error for a misregistered pool.
func RequireDaemon(env Env) (DaemonEnv, error) {
	d, ok := env.(DaemonEnv)
	if !ok {
		return nil, errs.New(errs.CodeInternal, "this node must run in the daemon pool")
	}
	return d, nil
}

// AIEnv is what pool ai nodes get: clients of the Python AI worker (I6). The
// worker builds them from its connector registry.
type AIEnv interface {
	Env
	Transcriber(ctx context.Context) (connector.Transcriber, error)
	Embedder(ctx context.Context) (connector.Embedder, error)
}

// RequireAI returns the AIEnv or a clear error for a misregistered pool.
func RequireAI(env Env) (AIEnv, error) {
	a, ok := env.(AIEnv)
	if !ok {
		return nil, errs.New(errs.CodeInternal, "this node must run in the ai pool")
	}
	return a, nil
}

// IdempotencyKey is stable across attempts of the same node in the same job
// (I36), so retries never look like new external effects. A flow.foreach item
// runs as its own child job, which gives it its own key.
func IdempotencyKey(tenant domain.TenantID, job id.ULID, nodeID string) string {
	sum := sha256.Sum256([]byte(string(tenant) + "\x00" + string(job) + "\x00" + nodeID))
	return hex.EncodeToString(sum[:])
}

// Registrar is what a node package's Register function receives.
type Registrar interface {
	RegisterNode(n Node)
}

// Registry resolves node types. NewRegistry returns the standard one.
type Registry interface {
	Registrar
	Lookup(nodeType string, version int) (Node, bool)
	List() []Descriptor
}

type registry struct {
	mu    sync.RWMutex
	nodes map[string]Node
}

// NewRegistry returns an empty registry. Registering a type and version twice panics.
func NewRegistry() Registry { return &registry{nodes: map[string]Node{}} }

func nodeKey(nodeType string, version int) string { return fmt.Sprintf("%s@%d", nodeType, version) }

func (r *registry) RegisterNode(n Node) {
	d := n.Descriptor()
	key := nodeKey(d.Type, d.TypeVersion)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.nodes[key]; dup {
		panic("node: duplicate type " + key)
	}
	r.nodes[key] = n
}

func (r *registry) Lookup(nodeType string, version int) (Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[nodeKey(nodeType, version)]
	return n, ok
}

func (r *registry) List() []Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Descriptor, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, n.Descriptor())
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Type != out[b].Type {
			return out[a].Type < out[b].Type
		}
		return out[a].TypeVersion < out[b].TypeVersion
	})
	return out
}
