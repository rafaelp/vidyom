// Package plugin defines the plugin manifest (§7.6, D43) and the JSON-RPC 2.0
// method set plugins speak over stdin/stdout (D3, I42). A plugin is a
// distributable package that can add nodes, connectors, hooks and event
// subscribers. schemas/plugin-manifest.v1.json validates manifests.
//
// In this version only hook actions and event subscriptions are activated; a
// manifest providing nodes or connector drivers is refused at install (I269).
package plugin

import (
	"context"
	"encoding/json"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/node"
)

// ManifestFile is the manifest's file name in a plugin directory.
const ManifestFile = "vidyom-plugin.json"

// ProtocolVersion is this contract's version.
const ProtocolVersion = 1

// Runtime is how a plugin runs. Remote validates and refuses to start in this
// version (I42). WASM is Future.
type Runtime string

const (
	RuntimeProcess Runtime = "process"
	RuntimeRemote  Runtime = "remote"
)

// Provides lists what a plugin adds.
type Provides struct {
	StorageDrivers     []string `json:"storageDrivers,omitempty"`
	PublishingDrivers  []string `json:"publishingDrivers,omitempty"`
	CatalogDrivers     []string `json:"catalogDrivers,omitempty"`
	LogSinkDrivers     []string `json:"logSinkDrivers,omitempty"`
	NodeTypes          []string `json:"nodeTypes,omitempty"`
	HookActions        []string `json:"hookActions,omitempty"`
	EventSubscriptions []string `json:"eventSubscriptions,omitempty"`
}

// Permissions are explicit; nothing is granted automatically (§7.6, §9.4).
type Permissions struct {
	Network    []string `json:"network,omitempty"`
	Secrets    []string `json:"secrets,omitempty"`
	Filesystem []string `json:"filesystem,omitempty"` // "job-temp" and declared paths
	Events     []string `json:"events,omitempty"`
}

// Entrypoint is what to execute for a process plugin, relative to its directory.
// There is no shell.
type Entrypoint struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// Manifest is §7.6 plus the entrypoint the runtime needs.
type Manifest struct {
	ID          string            `json:"id"`
	Version     string            `json:"version"`
	Runtime     Runtime           `json:"runtime"`
	Entrypoint  *Entrypoint       `json:"entrypoint,omitempty"`
	Provides    Provides          `json:"provides"`
	Permissions Permissions       `json:"permissions"`
	Contracts   map[string]string `json:"contracts,omitempty"`
	SHA256      string            `json:"sha256,omitempty"` // of the entrypoint, verified at load (§9.4)
}

// RPC methods the daemon calls on a plugin.
const (
	MethodInitialize   = "initialize"
	MethodShutdown     = "shutdown"
	MethodHealth       = "health"
	MethodNodeDescribe = "node.describe"
	MethodNodeExecute  = "node.execute"
	MethodHookInvoke   = "hook.invoke"
	MethodEventDeliver = "event.deliver"
	MethodConnector    = "connector.invoke"
)

// RPC methods a plugin calls on the daemon (I266). Each is checked against the
// process's effective permissions.
const (
	// MethodHostSecret is a request for one granted secret. HostSecretParams to
	// HostSecretResult. A name outside the effective list answers
	// jsonrpc.CodeUnauthorized and is audited.
	MethodHostSecret = "host.secret"
	// MethodHostLog is a notification carrying one HostLogParams. It goes through
	// redaction like any other log.
	MethodHostLog = "host.log"
)

// HostSecretParams names the secret. Plugins never receive a secret any other way.
type HostSecretParams struct {
	Name string `json:"name"`
}

// HostSecretResult holds the value. It stays on the stdin pipe and in memory.
type HostSecretResult struct {
	Value string `json:"value"`
}

// HostLogParams is one structured log line from a plugin.
type HostLogParams struct {
	Level   string         `json:"level,omitempty"` // debug, info, warn, error
	Event   string         `json:"event,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// InitializeParams starts a plugin session. A process serves one tenant (I262).
// Permissions are the effective ones: the manifest's, narrowed by the tenant's
// grant (I261). TempDir is empty unless job-temp was granted.
type InitializeParams struct {
	ProtocolVersion int             `json:"protocol_version"`
	Permissions     Permissions     `json:"permissions"`
	TenantID        domain.TenantID `json:"tenant_id,omitempty"`
	TempDir         string          `json:"temp_dir,omitempty"`
}

// InitializeResult is the plugin's answer.
type InitializeResult struct {
	ProtocolVersion int      `json:"protocol_version"`
	PluginID        string   `json:"plugin_id"`
	Provides        Provides `json:"provides"`
}

// NodeDescribeResult lists the plugin's node descriptors.
type NodeDescribeResult struct {
	Nodes []node.Descriptor `json:"nodes"`
}

// NodeExecuteParams runs a plugin node. Secrets are never included.
type NodeExecuteParams struct {
	NodeType       string          `json:"node_type"`
	TypeVersion    int             `json:"type_version"`
	Parameters     json.RawMessage `json:"parameters"`
	Attempt        int             `json:"attempt"`
	IdempotencyKey string          `json:"idempotency_key"`
	TenantID       string          `json:"tenant_id"`
	JobID          string          `json:"job_id"`
	ScratchDir     string          `json:"scratch_dir,omitempty"`
}

// NodeExecuteResult is the node's output.
type NodeExecuteResult struct {
	Output json.RawMessage `json:"output"`
}

// HookInvokeParams is a hook call; hook.Result is the response.
type HookInvokeParams = hook.Invocation

// EventDeliverParams delivers an event at least once; EventID is the idempotency
// key (§8.2).
type EventDeliverParams = event.Envelope

// ConnectorInvokeParams calls one connector operation of a plugin driver, by name.
// File data goes by local path.
type ConnectorInvokeParams struct {
	Family    string          `json:"family"`
	Driver    string          `json:"driver"`
	Operation string          `json:"operation"`
	Params    json.RawMessage `json:"params"`
}

// EventSubscriber is how the Event Bus reaches plugins (§8.2, I270). Implemented
// by component extensibility, called by component engine, which records deliveries
// as plugin:<id> and retries on error. EventID is the idempotency key, because
// delivery is at-least-once.
type EventSubscriber interface {
	// EventSubscribers returns the plugin IDs that may receive this type in this
	// tenant: declared in the manifest and granted by the tenant.
	EventSubscribers(ctx context.Context, tenant domain.TenantID, eventType event.Type) ([]string, error)
	// DeliverEvent hands one event to one plugin, checking the grant again.
	DeliverEvent(ctx context.Context, pluginID string, e event.Envelope) error
}
