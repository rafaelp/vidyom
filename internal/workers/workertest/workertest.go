// Package workertest runs nodes against a real task environment: real scratch, a
// real encrypted cache and the real local storage driver on a temporary tree, with a
// small stand-in for the daemon's lease services. It checks every execution against
// the node catalog, so a node cannot drift from its contract. Only tests import it.
package workertest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rafaelp/vidyom/connectors/storage/local"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
	"github.com/rafaelp/vidyom/internal/workers"
	"github.com/rafaelp/vidyom/internal/workers/cache"
	"github.com/rafaelp/vidyom/schemas"
)

// Services stands in for the daemon's lease services.
type Services struct {
	mu        sync.Mutex
	Instances map[domain.StorageRole]config.StorageInstance
	Key       []byte
	// ConnectionCalls counts Connection calls per role.
	ConnectionCalls map[domain.StorageRole]int
	KeyCalls        atomic.Int64
}

// Connection returns a role's instance.
func (s *Services) Connection(_ context.Context, _ string, req workerproto.ConnectionRequest) (workerproto.ConnectionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ConnectionCalls[req.Role]++
	inst, ok := s.Instances[req.Role]
	if !ok {
		return workerproto.ConnectionResponse{}, errs.Newf(errs.CodeFailedPrecondition, "role %s is not configured", req.Role)
	}
	return workerproto.ConnectionResponse{Instance: inst}, nil
}

// CacheKey returns the tenant's cache key.
func (s *Services) CacheKey(context.Context, string, workerproto.CacheKeyRequest) (workerproto.CacheKeyResponse, error) {
	s.KeyCalls.Add(1)
	return workerproto.CacheKeyResponse{Key: s.Key}, nil
}

// Harness is a temporary tree for one test's job.
type Harness struct {
	T         *testing.T
	Root      string
	Tenant    domain.TenantID
	Job       id.ULID
	Services  *Services
	Deps      workers.TaskDeps
	AccessLog string
	// Permissions are granted to the job.
	Permissions []domain.Permission
}

// New builds a harness: local roles archive, master and proxy, all logging access.
func New(t *testing.T) *Harness {
	t.Helper()
	root := t.TempDir()
	h := &Harness{
		T:         t,
		Root:      root,
		Tenant:    "acme",
		Job:       id.New(),
		AccessLog: filepath.Join(root, "access.jsonl"),
	}
	params, err := json.Marshal(map[string]any{"accessLog": h.AccessLog})
	if err != nil {
		t.Fatal(err)
	}
	inst := func(role string, usage config.StorageUsage) config.StorageInstance {
		return config.StorageInstance{Driver: local.Driver, Root: filepath.Join(root, "roles", role), Usage: usage, Parameters: params}
	}
	h.Services = &Services{
		Instances: map[domain.StorageRole]config.StorageInstance{
			domain.RoleArchive: inst("archive", config.UsageBackupOnly),
			domain.RoleMaster:  inst("master", config.UsageOperational),
			domain.RoleProxy:   inst("proxy", config.UsageOperational),
		},
		Key:             []byte("0123456789abcdef0123456789abcdef"),
		ConnectionCalls: map[domain.StorageRole]int{},
	}
	registry := connector.NewRegistry()
	local.Register(registry)
	tenantCache, err := cache.New(cache.Options{Root: filepath.Join(root, "cache")})
	if err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(root, "spool")
	if err := os.MkdirAll(filepath.Join(spool, string(h.Tenant)), 0o700); err != nil {
		t.Fatal(err)
	}
	h.Deps = workers.TaskDeps{
		Services:   h.Services,
		Connectors: registry,
		Cache:      tenantCache,
		ScratchDir: filepath.Join(root, "scratch"),
		SpoolDir:   spool,
	}
	return h
}

// Lease builds a lease for a node of this job.
func (h *Harness) Lease(nodeID, nodeType string) workerproto.Lease {
	return workerproto.Lease{
		TaskID:         h.TaskID(nodeID),
		LeaseID:        "lease-" + nodeID,
		TenantID:       h.Tenant,
		JobID:          "JOB-001",
		JobULID:        h.Job,
		NodeID:         nodeID,
		NodeType:       nodeType,
		TypeVersion:    1,
		Attempt:        1,
		IdempotencyKey: node.IdempotencyKey(h.Tenant, h.Job, nodeID),
		Permissions:    h.Permissions,
	}
}

// TaskID returns the task ID of a node of this job.
func (h *Harness) TaskID(nodeID string) string { return string(h.Job) + "." + nodeID + ".1" }

// Env builds a task environment for a node ID, closed when the test ends.
func (h *Harness) Env(nodeID, nodeType string) *workers.TaskEnv {
	h.T.Helper()
	env, err := workers.NewTaskEnv(h.Deps, h.Lease(nodeID, nodeType), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		h.T.Fatal(err)
	}
	h.T.Cleanup(env.Close)
	return env
}

// Outcome is a node run.
type Outcome struct {
	Result node.Result
	Output map[string]any
}

// Field returns a string output field.
func (o Outcome) Field(name string) string {
	v, _ := o.Output[name].(string)
	return v
}

// Number returns a numeric output field.
func (o Outcome) Number(name string) float64 {
	v, _ := o.Output[name].(float64)
	return v
}

// Run executes a node once with the given parameters. Parameters and output are
// checked against the catalog, and the descriptor against the node's declaration.
func (h *Harness) Run(ctx context.Context, n node.Node, nodeID string, params any) (Outcome, error) {
	h.T.Helper()
	desc := n.Descriptor()
	if err := schemas.CheckDescriptor(desc); err != nil {
		h.T.Fatal(err)
	}
	data, err := json.Marshal(params)
	if err != nil {
		h.T.Fatal(err)
	}
	catalog, err := schemas.NodeCatalog()
	if err != nil {
		h.T.Fatal(err)
	}
	entry, ok := catalog.Lookup(desc.Type, desc.TypeVersion)
	if !ok {
		h.T.Fatalf("%s is not in the catalog", desc.Type)
	}
	if err := schemas.Validate(entry.ParametersRef(), data); err != nil {
		h.T.Fatalf("%s parameters do not match the catalog: %v", desc.Type, err)
	}
	env := h.Env(nodeID, desc.Type)
	result, err := n.Execute(ctx, env, node.Input{
		Parameters:     data,
		Attempt:        1,
		IdempotencyKey: node.IdempotencyKey(h.Tenant, h.Job, nodeID),
		LeaseID:        "lease-" + nodeID,
	})
	if err != nil {
		return Outcome{}, err
	}
	if err := schemas.Validate(entry.OutputRef(), result.Output); err != nil {
		h.T.Fatalf("%s output does not match the catalog: %v\n%s", desc.Type, err, result.Output)
	}
	var out map[string]any
	if err := json.Unmarshal(result.Output, &out); err != nil {
		h.T.Fatal(err)
	}
	return Outcome{Result: result, Output: out}, nil
}

// MustRun is Run that fails the test on an error.
func (h *Harness) MustRun(ctx context.Context, n node.Node, nodeID string, params any) Outcome {
	h.T.Helper()
	o, err := h.Run(ctx, n, nodeID, params)
	if err != nil {
		h.T.Fatalf("%s: %v", n.Descriptor().Type, err)
	}
	return o
}

// Storage opens a role directly, outside any node.
func (h *Harness) Storage(role domain.StorageRole) connector.Storage {
	h.T.Helper()
	env := h.Env("harness-"+string(role), "harness")
	s, err := env.Storage(context.Background(), role)
	if err != nil {
		h.T.Fatal(err)
	}
	return s
}

// PutFile stores a local file in a role, as an earlier ingest would have.
func (h *Harness) PutFile(role domain.StorageRole, key, path string) connector.ObjectInfo {
	h.T.Helper()
	f, err := os.Open(path)
	if err != nil {
		h.T.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := h.Storage(role).Put(context.Background(), key, f, connector.PutOptions{})
	if err != nil {
		h.T.Fatal(err)
	}
	return info
}

// ScratchPath resolves a scratch URI of this job to its path.
func (h *Harness) ScratchPath(uri string) string {
	h.T.Helper()
	parsed, err := domain.ParseInputURI(uri)
	if err != nil {
		h.T.Fatal(err)
	}
	if parsed.Kind != domain.URIScratch {
		h.T.Fatalf("%s is not a scratch reference", uri)
	}
	return filepath.Join(h.Deps.ScratchDir, string(h.Tenant), parsed.JobULID, parsed.NodeID, filepath.FromSlash(parsed.File))
}

// AccessEntry is one line of the local driver's access log.
type AccessEntry struct {
	Tenant string `json:"tenant"`
	Role   string `json:"role"`
	Op     string `json:"op"`
	Class  string `json:"class"`
	Key    string `json:"key"`
	Bytes  int64  `json:"bytes"`
}

// Access reads the access log so far.
func (h *Harness) Access() []AccessEntry {
	h.T.Helper()
	f, err := os.Open(h.AccessLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.T.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []AccessEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry AccessEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			h.T.Fatalf("bad access line %q: %v", scanner.Text(), err)
		}
		out = append(out, entry)
	}
	return out
}

// ResetAccess empties the access log, to scope a later assertion.
func (h *Harness) ResetAccess() {
	h.T.Helper()
	if err := os.WriteFile(h.AccessLog, nil, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		h.T.Fatal(err)
	}
}

// ArchiveReads returns the media reads of the archive role: the entries that §3.2
// says clipping must never produce (I188).
func (h *Harness) ArchiveReads() []AccessEntry {
	h.T.Helper()
	var out []AccessEntry
	for _, entry := range h.Access() {
		if entry.Role == string(domain.RoleArchive) && entry.Class == "read" {
			out = append(out, entry)
		}
	}
	return out
}

// Reads returns the media reads of a role.
func (h *Harness) Reads(role domain.StorageRole) []AccessEntry {
	h.T.Helper()
	var out []AccessEntry
	for _, entry := range h.Access() {
		if entry.Role == string(role) && entry.Class == "read" {
			out = append(out, entry)
		}
	}
	return out
}

// Contains reports whether a file holds a string. Used to prove what is not on disk.
func Contains(t *testing.T, path, needle string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(data), needle)
}
