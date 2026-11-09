// Package config holds configuration types: the daemon's own settings and each
// tenant's connector settings (§7.2, §7.3, §7.4, I34). Files only ever contain
// credentialsRef references, never secret values (§9.1). JSON keys are
// camelCase as in the spec's examples.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// Daemon is <config-dir>/daemon.json.
type Daemon struct {
	DataDir    string `json:"dataDir"`    // holds system.db and tenants/ (§14.1)
	CacheDir   string `json:"cacheDir"`   // worker cache root (D22)
	SpoolDir   string `json:"spoolDir"`   // upload spool, shared with workers (I20)
	ScratchDir string `json:"scratchDir"` // job scratch, shared by workers (I5)

	Listeners     Listeners     `json:"listeners"`
	Redis         Redis         `json:"redis"`
	Vault         Vault         `json:"vault"`
	IngestRoots   []string      `json:"ingestRoots,omitempty"` // server: inputs (R20)
	LocalUsers    []LocalUser   `json:"localUsers,omitempty"`  // Unix peer identity (I17)
	AIWorker      AIWorker      `json:"aiWorker"`
	Retention     Retention     `json:"retention"`
	Observability Observability `json:"observability"`
	Workflows     WorkflowDirs  `json:"workflows"`
}

// Listeners configures how clients reach the daemon. There is no implicit
// listener besides the Unix socket when enabled, and any failure aborts startup
// rather than falling back (§9.3, I19).
type Listeners struct {
	Unix  *UnixListener  `json:"unix,omitempty"`
	HTTPS *HTTPSListener `json:"https,omitempty"`
}

// UnixListener is the same-host transport (D49).
type UnixListener struct {
	Path string `json:"path"`
	Mode string `json:"mode,omitempty"` // default "0600"
}

// HTTPSListener is the remote transport, TLS 1.3 minimum (D12). Exactly one of
// Bind or Interface is required. Interface names an overlay interface
// (tailscale0, ...) whose absence aborts startup (§9.3).
type HTTPSListener struct {
	Bind        string `json:"bind,omitempty"`
	Interface   string `json:"interface,omitempty"`
	Port        int    `json:"port"`
	AllowPublic bool   `json:"allowPublic,omitempty"` // required for 0.0.0.0 or ::
	TLS         TLS    `json:"tls"`
}

// TLSMode selects certificate handling (I18).
type TLSMode string

const (
	TLSAuto  TLSMode = "auto"  // self-signed, loopback only, pinned by fingerprint
	TLSFiles TLSMode = "files" // operator-provided certificate
)

// TLS configures the HTTPS listener.
type TLS struct {
	Mode         TLSMode `json:"mode"`
	CertFile     string  `json:"certFile,omitempty"`
	KeyFile      string  `json:"keyFile,omitempty"`
	ClientCAFile string  `json:"clientCAFile,omitempty"` // mTLS when set
}

// LocalUser maps a Unix UID to a Vidyom user.
type LocalUser struct {
	UID   int    `json:"uid"`
	Email string `json:"email"`
}

// Redis points to the required job queue (D4).
type Redis struct {
	URL string `json:"url"`
}

// UnsealProvider names a master key provider (I10). There is no default.
type UnsealProvider string

const (
	UnsealSystemdCreds  UnsealProvider = "systemd-creds"
	UnsealMacOSKeychain UnsealProvider = "macos-keychain"
	UnsealDPAPI         UnsealProvider = "dpapi"
	UnsealGCPKMS        UnsealProvider = "gcp-kms"
	UnsealFile          UnsealProvider = "file"
	UnsealEnv           UnsealProvider = "env"
)

// Vault configures master key unsealing. A missing provider means the daemon
// exits with no listener (D13).
type Vault struct {
	Unseal Unseal `json:"unseal"`
}

// Unseal holds provider options. No option contains key material.
type Unseal struct {
	Provider UnsealProvider `json:"provider"`
	// File provider: path of the key, typically a Compose secret. The operator
	// must acknowledge that it is not hardware sealed.
	Path                  string `json:"path,omitempty"`
	AcknowledgeFileSealed bool   `json:"acknowledgeFileSealed,omitempty"`
	// KMS provider.
	KMSKeyName string `json:"kmsKeyName,omitempty"`
	WrappedKey string `json:"wrappedKeyPath,omitempty"`
	// macOS Keychain and systemd-creds item name.
	ItemName string `json:"itemName,omitempty"`
}

// AIWorker is how clients reach the Python AI worker (I6): unix:///path or
// tcp://127.0.0.1:port. TokenFile holds the initialize token for the tcp form.
type AIWorker struct {
	Endpoint  string `json:"endpoint"`
	TokenFile string `json:"tokenFile,omitempty"`
}

// Retention holds periods of §10.4, D16 and D40. Zero means the default.
type Retention struct {
	LogDays              int   `json:"logDays,omitempty"`
	JobDays              int   `json:"jobDays,omitempty"`
	AuditDays            int   `json:"auditDays,omitempty"`
	UnapprovedRenderDays int   `json:"unapprovedRenderDays,omitempty"`
	CacheIdleDays        int   `json:"cacheIdleDays,omitempty"`
	CacheMaxBytes        int64 `json:"cacheMaxBytes,omitempty"`
}

// Retention defaults (§10.4, D16, D22).
const (
	DefaultLogDays              = 30
	DefaultJobDays              = 90
	MinAuditDays                = 365
	DefaultUnapprovedRenderDays = 30
	DefaultCacheIdleDays        = 7
)

// WorkflowDirs lists directories that add or override workflow files (I22).
type WorkflowDirs struct {
	Dirs []string `json:"dirs,omitempty"`
}

// ---- Tenant configuration ----

// Tenant is the assembled configuration of one tenant.
type Tenant struct {
	Storages      Storages      `json:"storages"`
	Publishing    Publishing    `json:"publishing"`
	AI            AI            `json:"ai"`
	Catalogs      Catalogs      `json:"catalogs"`
	Observability Observability `json:"observability"`
	Hooks         Hooks         `json:"hooks"`
	Budget        BudgetCosts   `json:"budget"`
}

// TenantSource supplies a tenant's configuration. Implemented by component core.
type TenantSource interface {
	TenantConfig(ctx context.Context, tenant domain.TenantID) (Tenant, error)
}

// StorageUsage is the direct-access policy declared on a storage instance (§7.2).
type StorageUsage string

const (
	UsageBackupOnly  StorageUsage = "backup_only"
	UsageOperational StorageUsage = "operational"
)

// ArchivePolicy is the versions-to-archive policy (R29, D16). Raw originals are
// always archived regardless.
type ArchivePolicy string

const (
	ArchiveAllVersions      ArchivePolicy = "all_versions"
	ArchiveApprovedVersions ArchivePolicy = "approved_versions"
	ArchiveLatest           ArchivePolicy = "latest"
)

// StorageInstance is a role's configured instance (§7.2). `root` is used by the
// local driver.
type StorageInstance struct {
	Driver         string          `json:"driver"`
	Bucket         string          `json:"bucket,omitempty"`
	Root           string          `json:"root,omitempty"`
	Prefix         string          `json:"prefix,omitempty"`
	StorageClass   string          `json:"storageClass,omitempty"`
	CredentialsRef string          `json:"credentialsRef,omitempty"`
	Usage          StorageUsage    `json:"usage,omitempty"`
	Parameters     json.RawMessage `json:"parameters,omitempty"`
}

// Storages is storages.json: instance per role.
type Storages struct {
	Roles         map[domain.StorageRole]StorageInstance `json:"storages"`
	ArchivePolicy ArchivePolicy                          `json:"archivePolicy,omitempty"`
}

// ValidateStorages enforces R28: roles occupy disjoint namespaces, so a proxy
// can never sit "inside" the master and role validation stays possible. It also
// requires all three roles and refuses any reference that looks like an inline
// credential. The check is per tenant, after prefixes apply.
func ValidateStorages(s Storages) error {
	type ns struct {
		role     domain.StorageRole
		location string
		prefix   string
	}
	var spaces []ns
	for _, role := range domain.StorageRoles {
		inst, ok := s.Roles[role]
		if !ok {
			return fmt.Errorf("storage role %q is not configured", role)
		}
		if inst.Driver == "" {
			return fmt.Errorf("storage role %q has no driver", role)
		}
		location := inst.Bucket
		if location == "" {
			location = inst.Root
		}
		if location == "" {
			return fmt.Errorf("storage role %q has neither bucket nor root", role)
		}
		if strings.ContainsAny(inst.CredentialsRef, " \t\r\n") {
			return fmt.Errorf("storage role %q: credentialsRef must be a reference name", role)
		}
		spaces = append(spaces, ns{role, inst.Driver + ":" + path.Clean(location), normalizePrefix(inst.Prefix)})
	}
	sort.Slice(spaces, func(i, j int) bool { return spaces[i].role < spaces[j].role })
	for i, a := range spaces {
		for _, b := range spaces[i+1:] {
			if overlaps(a.location, a.prefix, b.location, b.prefix) {
				return fmt.Errorf("storage roles %q and %q have overlapping namespaces (R28)", a.role, b.role)
			}
		}
	}
	return nil
}

func normalizePrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// overlaps treats local roots hierarchically: /data and /data/x overlap too.
func overlaps(locA, prefixA, locB, prefixB string) bool {
	fullA := strings.TrimRight(locA, "/") + "/" + prefixA
	fullB := strings.TrimRight(locB, "/") + "/" + prefixB
	return strings.HasPrefix(fullA, fullB) || strings.HasPrefix(fullB, fullA)
}

// TenantStoragePrefix returns a tenant's key prefix inside a role (D6).
func TenantStoragePrefix(tenant domain.TenantID) string { return string(tenant) + "/" }

// Destination is one publishing destination (§7.3).
type Destination struct {
	Driver                           string            `json:"driver"`
	CredentialsRef                   string            `json:"credentialsRef,omitempty"`
	Visibility                       domain.Visibility `json:"visibility,omitempty"`
	AllowPrivateUploadBeforeApproval *bool             `json:"allowPrivateUploadBeforeApproval,omitempty"`
	Parameters                       json.RawMessage   `json:"parameters,omitempty"`
}

// PublishingDefaults are publishing.defaults (§7.3).
type PublishingDefaults struct {
	RequireApproval                  bool `json:"requireApproval"`
	AllowPrivateUploadBeforeApproval bool `json:"allowPrivateUploadBeforeApproval"`
	UploadResumable                  bool `json:"uploadResumable"`
	SavePublicationRecord            bool `json:"savePublicationRecord"`
}

// Publishing is publishing.json. Its top-level key is `publishing` in the file.
type Publishing struct {
	Destinations map[string]Destination `json:"destinations"`
	Defaults     PublishingDefaults     `json:"defaults"`
}

// EndpointClass classifies an LLM endpoint for the external AI policy (D8, D9).
type EndpointClass string

const (
	EndpointLocal    EndpointClass = "local"
	EndpointExternal EndpointClass = "external"
)

// LLMEndpoint is one OpenAI-compatible endpoint (D9).
type LLMEndpoint struct {
	Name           string        `json:"name"`
	BaseURL        string        `json:"baseUrl"`
	Model          string        `json:"model"`
	Class          EndpointClass `json:"class"`
	CredentialsRef string        `json:"credentialsRef,omitempty"`
}

// AI is ai.json.
type AI struct {
	DefaultProcessing  domain.AIProcessing `json:"defaultProcessing,omitempty"` // default local_only (D8)
	LLMEndpoints       []LLMEndpoint       `json:"llmEndpoints,omitempty"`
	EmbeddingModel     string              `json:"embeddingModel,omitempty"`     // default "bge-m3"
	TranscriptionModel string              `json:"transcriptionModel,omitempty"` // default "large-v3"
}

// AllowedEndpoints applies D8: local_only content may only use endpoints classified
// local; external_allowed may use any, local first. Every path that sends content to
// an LLM selects through this function.
func AllowedEndpoints(endpoints []LLMEndpoint, processing domain.AIProcessing) []LLMEndpoint {
	var local, external []LLMEndpoint
	for _, e := range endpoints {
		if e.Class == EndpointLocal {
			local = append(local, e)
		} else {
			external = append(external, e)
		}
	}
	if processing == domain.AIExternalAllowed {
		return append(local, external...)
	}
	return local
}

// CatalogInstance configures an optional external catalog connector (D24).
type CatalogInstance struct {
	Driver         string          `json:"driver"`
	CredentialsRef string          `json:"credentialsRef,omitempty"`
	Parameters     json.RawMessage `json:"parameters,omitempty"`
}

// Catalogs holds optional catalogs and the public library destination (R44).
type Catalogs struct {
	External      map[string]CatalogInstance `json:"external,omitempty"`
	PublicLibrary *CatalogInstance           `json:"publicLibrary,omitempty"`
}

// Sink is a log sink, metrics exporter or trace exporter entry (§7.4, D45).
type Sink struct {
	ID             string          `json:"id"`
	Driver         string          `json:"driver"`
	Enabled        bool            `json:"enabled"`
	CredentialsRef string          `json:"credentialsRef,omitempty"`
	Parameters     json.RawMessage `json:"parameters,omitempty"`
}

// LogConfig is observability.logs.
type LogConfig struct {
	Level string `json:"level,omitempty"`
	Sinks []Sink `json:"sinks,omitempty"`
}

// Observability is observability.json. Its top-level key is `observability`.
type Observability struct {
	Logs    LogConfig `json:"logs"`
	Metrics []Sink    `json:"metrics,omitempty"`
	Traces  []Sink    `json:"traces,omitempty"`
}

// Hooks is hooks.json: global hooks, which workflows cannot remove (§8.1).
// MaxTimeoutSeconds is the configurable global ceiling (D40).
type Hooks struct {
	Workflow          wfdef.HookSet `json:"workflow"`
	Node              wfdef.HookSet `json:"node"`
	MaxTimeoutSeconds int           `json:"maxTimeoutSeconds,omitempty"`
}

// PublishingFile is the on-disk wrapper of publishing.json (§7.3).
type PublishingFile struct {
	Publishing Publishing `json:"publishing"`
}

// ObservabilityFile is the on-disk wrapper of observability.json (§7.4).
type ObservabilityFile struct {
	Observability Observability `json:"observability"`
}

// BudgetCosts is the estimated cost table per operation (D19, I8), in minor
// units per the operation's unit.
type BudgetCosts struct {
	PerUnitMinor map[string]float64 `json:"perUnitMinor,omitempty"`
}
