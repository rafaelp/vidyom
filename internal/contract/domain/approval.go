package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/id"
)

// ApprovalKind is the kind of a human gate (R13, R25, I41).
type ApprovalKind string

const (
	ApprovalClipSelection    ApprovalKind = "clip_selection"
	ApprovalPublication      ApprovalKind = "publication"
	ApprovalVisibilityChange ApprovalKind = "visibility_change"
	ApprovalMetadataChange   ApprovalKind = "metadata_change"
	ApprovalOther            ApprovalKind = "other"
)

// ApprovalStatus is the state of an approval record.
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
	ApprovalExpired  ApprovalStatus = "expired"
)

// ApprovalOnTimeout is the approval.request onTimeout parameter (§6.2).
type ApprovalOnTimeout string

const (
	OnTimeoutReject ApprovalOnTimeout = "reject"
	OnTimeoutFail   ApprovalOnTimeout = "fail"
	OnTimeoutRemind ApprovalOnTimeout = "remind"
)

// Approval is a durable gate with owner, date and approved version (R13).
// Evidence holds what the reviewer needs (D51): excerpt, timestamps, confidence
// and alternatives when ambiguous.
type Approval struct {
	ID             id.Friendly       `json:"id"`
	ULID           id.ULID           `json:"ulid"`
	TenantID       TenantID          `json:"tenant_id"`
	Kind           ApprovalKind      `json:"kind"`
	Status         ApprovalStatus    `json:"status"`
	AssetID        id.Friendly       `json:"asset_id"`
	AssetVersion   string            `json:"asset_version,omitempty"`
	JobID          id.Friendly       `json:"job_id"`
	NodeID         string            `json:"node_id"`
	Approvers      []string          `json:"approvers,omitempty"`
	Evidence       json.RawMessage   `json:"evidence,omitempty"`
	Preview        string            `json:"preview,omitempty"`
	AllowAdjust    bool              `json:"allow_adjust"`
	Segments       []Segment         `json:"segments,omitempty"` // approved, possibly adjusted
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	OnTimeout      ApprovalOnTimeout `json:"on_timeout,omitempty"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty"`
	DecidedBy      string            `json:"decided_by,omitempty"`
	DecidedAt      *time.Time        `json:"decided_at,omitempty"`
	Reason         string            `json:"reason,omitempty"`
	CreatedAt      time.Time         `json:"created_at,omitzero"`
}

// RequiredPermission returns the floor permission to decide this kind (D7).
// clip_selection needs editor or higher; publication and changes to an already
// public video need publisher or admin. Kind other requires explicit approvers
// and is decided with the clip_selection floor.
func (k ApprovalKind) RequiredPermission() Permission {
	switch k {
	case ApprovalPublication, ApprovalVisibilityChange, ApprovalMetadataChange:
		return PermApprovalDecidePublication
	default:
		return PermApprovalDecideClipSelection
	}
}

// ValidateApprovers checks a workflow's approvers list against the kind's
// floor. A workflow can tighten the rule, never loosen it (D7). Entries are
// role names or user emails.
func ValidateApprovers(kind ApprovalKind, approvers []string) error {
	if kind == ApprovalOther && len(approvers) == 0 {
		return fmt.Errorf("approval kind %q requires explicit approvers", kind)
	}
	for _, a := range approvers {
		role := Role(a)
		if !role.Valid() {
			continue // a user email; checked for the permission at decision time
		}
		if !role.Has(kind.RequiredPermission()) {
			return fmt.Errorf("approver role %q would loosen approval kind %q", a, kind)
		}
	}
	return nil
}

// MayDecide applies D7 at decision time: the caller must hold the kind's floor
// permission, and when the workflow lists approvers, the caller must match one
// of them by email or by a role at or above a listed role.
func MayDecide(p Principal, kind ApprovalKind, approvers []string) bool {
	if !p.Can(kind.RequiredPermission()) {
		return false
	}
	if len(approvers) == 0 {
		return true
	}
	return slices.ContainsFunc(approvers, func(a string) bool {
		if a == p.Email {
			return true
		}
		listed := Role(a)
		return listed.Valid() && p.Role.AtLeast(listed)
	})
}
