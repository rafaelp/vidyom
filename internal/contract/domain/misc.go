package domain

import (
	"encoding/json"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/id"
)

// UploadStatus is the state of a resumable upload (§5.4).
type UploadStatus string

const (
	UploadPending  UploadStatus = "pending"
	UploadComplete UploadStatus = "complete"
	UploadFailed   UploadStatus = "failed"
	UploadExpired  UploadStatus = "expired"
)

// Upload is a resumable client upload, UPL-nnn (D55).
type Upload struct {
	ID            id.Friendly  `json:"id"`
	ULID          id.ULID      `json:"ulid"`
	TenantID      TenantID     `json:"tenant_id"`
	Filename      string       `json:"filename"`
	SizeBytes     int64        `json:"size_bytes"`
	SHA256        string       `json:"sha256"`
	ChunkBytes    int64        `json:"chunk_bytes"`
	ReceivedBytes int64        `json:"received_bytes"`
	Status        UploadStatus `json:"status"`
	CreatedBy     string       `json:"created_by,omitempty"`
	CreatedAt     time.Time    `json:"created_at,omitzero"`
	ExpiresAt     time.Time    `json:"expires_at,omitzero"`
}

// SecretLocation says where a secret's value lives (§9.1).
type SecretLocation string

const (
	SecretClient    SecretLocation = "client"
	SecretServer    SecretLocation = "server"
	SecretDelegated SecretLocation = "delegated"
)

// SecretMeta is what the API ever returns about a secret; the value never leaves
// through it (R41).
type SecretMeta struct {
	Name       string         `json:"name"`
	TenantID   TenantID       `json:"tenant_id"`
	Location   SecretLocation `json:"location"`
	CreatedBy  string         `json:"created_by,omitempty"`
	CreatedAt  time.Time      `json:"created_at,omitzero"`
	RotatedAt  *time.Time     `json:"rotated_at,omitempty"`
	ExpiresAt  *time.Time     `json:"expires_at,omitempty"`
	LastUsedAt *time.Time     `json:"last_used_at,omitempty"`
}

// Budget is a tenant's monthly estimated budget (D19). Amounts are in minor
// currency units to avoid floating point.
type Budget struct {
	TenantID     TenantID `json:"tenant_id"`
	MonthlyMinor int64    `json:"monthly_minor"`
	Currency     string   `json:"currency"`
}

// UsageRecord is one estimated cost entry (§10.3).
type UsageRecord struct {
	TenantID       TenantID  `json:"tenant_id,omitempty"`
	Operation      string    `json:"operation"` // "storage.egress", "ai.external", ...
	Provider       string    `json:"provider,omitempty"`
	Quantity       float64   `json:"quantity"`
	Unit           string    `json:"unit,omitempty"`
	EstimatedMinor int64     `json:"estimated_minor"`
	JobID          string    `json:"job_id,omitempty"`
	NodeID         string    `json:"node_id,omitempty"`
	At             time.Time `json:"at,omitzero"`
}

// BudgetStatus is the current month's position.
type BudgetStatus struct {
	Budget        Budget     `json:"budget"`
	Period        string     `json:"period"` // "2026-11"
	SpentMinor    int64      `json:"spent_minor"`
	Percent       float64    `json:"percent"`
	Blocked       bool       `json:"blocked"`
	OverrideUntil *time.Time `json:"override_until,omitempty"`
}

// AuditRecord is an entry of the tenant's append-only, hash-chained audit (D23).
// It records the fact of an action, never content. Hash covers the canonical
// record including PrevHash.
type AuditRecord struct {
	Seq        int64           `json:"seq"`
	ULID       id.ULID         `json:"ulid"`
	TenantID   TenantID        `json:"tenant_id"`
	At         time.Time       `json:"at"`
	Actor      string          `json:"actor"`
	Method     AuthMethod      `json:"method,omitempty"`
	Action     string          `json:"action"`
	Permission Permission      `json:"permission,omitempty"`
	Resource   string          `json:"resource,omitempty"`
	Result     string          `json:"result"` // "allowed", "denied", "failed"
	RequestID  string          `json:"request_id,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
	PrevHash   string          `json:"prev_hash"`
	Hash       string          `json:"hash"`
}

// AuditVerification is the result of checking the chain (§11.5).
type AuditVerification struct {
	OK          bool   `json:"ok"`
	Records     int64  `json:"records"`
	FirstBadSeq int64  `json:"first_bad_seq,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// PluginRecord is an installed plugin in the system.db registry.
type PluginRecord struct {
	ID          string          `json:"id"`
	Version     string          `json:"version"`
	Manifest    json.RawMessage `json:"manifest"`
	SHA256      string          `json:"sha256"`
	Enabled     bool            `json:"enabled"`
	InstalledBy string          `json:"installed_by,omitempty"`
	InstalledAt time.Time       `json:"installed_at,omitzero"`
}
