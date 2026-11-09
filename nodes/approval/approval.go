package approvalnodes

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/schemas"
)

// ReasonApproval is the suspension reason of a gate.
const ReasonApproval = "approval"

// Register adds approval.request.
func Register(r node.Registrar) {
	d, err := schemas.DescriptorFor(wfdef.TypeApprovalRequest, 1)
	if err != nil {
		panic(err)
	}
	r.RegisterNode(request{desc: d})
}

type request struct{ desc node.Descriptor }

func (n request) Descriptor() node.Descriptor { return n.desc }

type params struct {
	Kind           domain.ApprovalKind      `json:"kind"`
	AssetID        string                   `json:"assetId"`
	Approvers      []string                 `json:"approvers"`
	TimeoutSeconds int                      `json:"timeoutSeconds"`
	OnTimeout      domain.ApprovalOnTimeout `json:"onTimeout"`
	Evidence       json.RawMessage          `json:"evidence"`
	Preview        string                   `json:"preview"`
	AllowAdjust    bool                     `json:"allowAdjust"`
	Version        string                   `json:"version"`
	Summary        string                   `json:"summary"`
}

func suspend(a domain.Approval) node.Result {
	return node.Result{Suspend: &node.Suspension{Reason: ReasonApproval, ApprovalID: a.ID}}
}

// Execute creates the record, once, and suspends. An earlier record is reused unless
// it expired, which only a retried job finds: the retry then opens a new gate.
func (n request) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, err
	}
	var p params
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	if err := domain.ValidateApprovers(p.Kind, p.Approvers); err != nil {
		return node.Result{}, node.Permanent(errs.Wrap(errs.CodeValidationFailed, err, err.Error()))
	}
	store := d.Store()
	existing, found, err := store.Approvals().GetForNode(ctx, env.JobULID(), env.NodeID())
	if err != nil {
		return node.Result{}, node.Transient(err)
	}
	if found && existing.Status != domain.ApprovalExpired {
		return suspend(existing), nil
	}
	asset, err := store.Assets().Get(ctx, id.Ref(p.AssetID))
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	version := p.Version
	if version == "" {
		version = asset.CurrentVersion
	}
	onTimeout := p.OnTimeout
	if onTimeout == "" {
		onTimeout = domain.OnTimeoutFail
	}
	now := time.Now().UTC()
	a := domain.Approval{
		ULID:           id.New(),
		TenantID:       env.Tenant(),
		Kind:           p.Kind,
		Status:         domain.ApprovalPending,
		AssetID:        asset.ID,
		AssetVersion:   version,
		JobID:          env.JobID(),
		NodeID:         env.NodeID(),
		Approvers:      p.Approvers,
		Evidence:       withSummary(p.Evidence, p.Summary),
		Preview:        p.Preview,
		AllowAdjust:    p.AllowAdjust,
		Segments:       evidenceSegments(p.Evidence),
		TimeoutSeconds: p.TimeoutSeconds,
		OnTimeout:      onTimeout,
		CreatedAt:      now,
	}
	if p.TimeoutSeconds > 0 {
		at := now.Add(time.Duration(p.TimeoutSeconds) * time.Second)
		a.ExpiresAt = &at
	}
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		next, err := tx.IDs().Next(ctx, id.PrefixApproval)
		if err != nil {
			return err
		}
		a.ID = next
		if err := tx.Approvals().Create(ctx, a); err != nil {
			return err
		}
		return d.Emit(ctx, tx, event.Draft{
			Type:    event.ApprovalRequested,
			AssetID: string(asset.ID),
			Payload: map[string]any{"approval_id": a.ID, "kind": a.Kind, "node_id": a.NodeID},
		})
	})
	if err != nil {
		return node.Result{}, node.Transient(err)
	}
	return suspend(a), nil
}

// withSummary keeps the evidence as given, adding the summary for the reviewer.
func withSummary(evidence json.RawMessage, summary string) json.RawMessage {
	if summary == "" {
		return evidence
	}
	doc := map[string]json.RawMessage{}
	if len(evidence) > 0 {
		if err := json.Unmarshal(evidence, &doc); err != nil {
			doc = map[string]json.RawMessage{"value": evidence}
		}
	}
	text, err := json.Marshal(summary)
	if err != nil {
		return evidence
	}
	doc["summary"] = text
	out, err := json.Marshal(doc)
	if err != nil {
		return evidence
	}
	return out
}

// evidenceSegments reads the segments a gate proposes, when it proposes any.
func evidenceSegments(evidence json.RawMessage) []domain.Segment {
	var doc struct {
		Segments []domain.Segment `json:"segments"`
	}
	if len(evidence) == 0 || json.Unmarshal(evidence, &doc) != nil {
		return nil
	}
	return doc.Segments
}

// Resume implements node.Resumer. The record is the truth. A rejected gate completes
// with no segments: a business outcome that flow.if routes, and a render wired
// straight after it cannot start (I41, I142).
func (n request) Resume(ctx context.Context, env node.Env, _ node.Input, _ json.RawMessage) (node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, err
	}
	a, found, err := d.Store().Approvals().GetForNode(ctx, env.JobULID(), env.NodeID())
	if err != nil {
		return node.Result{}, node.Transient(err)
	}
	if !found {
		return node.Result{}, node.Permanent(errs.New(errs.CodeInternal, "the approval record is gone"))
	}
	switch a.Status {
	case domain.ApprovalPending:
		return suspend(a), nil
	case domain.ApprovalExpired:
		return node.Result{}, node.Permanent(errs.Newf(errs.CodeTimeout, "approval %s expired", a.ID))
	}
	out := map[string]any{
		"approval_id": a.ID,
		"decision":    a.Status,
		"decided_by":  a.DecidedBy,
	}
	if a.DecidedAt != nil {
		out["decided_at"] = a.DecidedAt.UTC().Format(time.RFC3339)
	}
	if a.Reason != "" {
		out["reason"] = a.Reason
	}
	if a.Status == domain.ApprovalApproved && len(a.Segments) > 0 {
		out["segments"] = a.Segments
	}
	return node.JSONResult(out)
}
