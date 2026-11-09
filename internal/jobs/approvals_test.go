package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// newAsset creates an asset the way catalog.register would.
func (h *harness) newAsset(kind domain.AssetKind) domain.Asset {
	h.t.Helper()
	ctx := context.Background()
	var out domain.Asset
	err := h.store.InTx(ctx, func(tx repo.TenantRepos) error {
		friendly, err := tx.IDs().Next(ctx, kind.IDPrefix())
		if err != nil {
			return err
		}
		out = domain.Asset{ID: friendly, ULID: id.New(), TenantID: "acme", Kind: kind, Title: "A video", CurrentVersion: "v1"}
		if err := tx.Assets().Create(ctx, out); err != nil {
			return err
		}
		return tx.Versions().Create(ctx, domain.AssetVersion{AssetID: friendly, Version: "v1"})
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

const gateDoc = `{"id":"gate","version":"1.0.0","inputs":{"asset":{"type":"string","required":true}},"nodes":[
	{"id":"ask","type":"approval.request","typeVersion":1,"parameters":{"kind":"KIND","assetId":"${inputs.asset}","allowAdjust":true,
		"evidence":{"segments":[{"start_seconds":10,"end_seconds":20}],"confidence":0.9,"excerpt":"text"},"preview":"scratch://01J00000000000000000000000/preview/p.mp4","summary":"pick this"}},
	{"id":"decided","type":"flow.if","typeVersion":1,"parameters":{"condition":"nodes.ask.output.decision == 'approved'"}},
	{"id":"tail","type":"test.pure","typeVersion":1,"parameters":{"segments":"${nodes.ask.output.segments}"}}
],"connections":{"ask":["decided"],"decided":["tail"]},"outputs":{"decision":"${nodes.ask.output.decision}","by":"${nodes.ask.output.decided_by}"}}`

func (h *harness) pendingApproval(j domain.Job) domain.Approval {
	h.t.Helper()
	h.waitStatus(j, domain.JobWaitingApproval)
	a, found, err := h.store.Approvals().GetForNode(context.Background(), j.ULID, "ask")
	if err != nil || !found {
		h.t.Fatalf("approval: %v %v", found, err)
	}
	return a
}

func TestApprovalLevelsD7(t *testing.T) {
	h := newHarness(t)
	asset := h.newAsset(domain.KindOriginal)
	h.add(strings.Replace(gateDoc, "KIND", "clip_selection", 1))
	ctx := context.Background()
	apps := h.engine.Approvals()

	// clip_selection: a viewer cannot; an editor can.
	j := h.runAs(editor, "gate", map[string]any{"asset": asset.ID})
	a := h.pendingApproval(j)
	if a.ID != "APR-001" || a.Status != domain.ApprovalPending || a.AssetID != asset.ID || a.AssetVersion != "v1" {
		t.Fatalf("approval %+v", a)
	}
	if countEvents(t, h.store, event.ApprovalRequested) != 1 {
		t.Fatal("approval.requested must fire")
	}
	var evidence map[string]any
	if err := json.Unmarshal(a.Evidence, &evidence); err != nil || evidence["summary"] != "pick this" || evidence["excerpt"] != "text" {
		t.Fatalf("evidence %s", a.Evidence)
	}
	if _, err := apps.Approve(ctx, scopeOf(viewer), id.Ref(a.ID), api.ApproveRequest{}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("viewer: %v", err)
	}
	decided, err := apps.Approve(ctx, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{Note: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if decided.Status != domain.ApprovalApproved || decided.DecidedBy != editor.Email || decided.DecidedAt == nil {
		t.Fatalf("decided %+v", decided)
	}
	done := h.waitStatus(j, domain.JobCompleted)
	var params map[string]any
	if err := json.Unmarshal(h.callsOf("tail")[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	segs := params["segments"].([]any)
	if len(segs) != 1 || segs[0].(map[string]any)["start_seconds"] != float64(10) || segs[0].(map[string]any)["source_asset_id"] != string(asset.ID) {
		t.Fatalf("segments %v", segs)
	}
	var outs map[string]any
	_ = json.Unmarshal(done.Outputs, &outs)
	if outs["decision"] != "approved" || outs["by"] != editor.Email {
		t.Fatalf("outputs %v", outs)
	}
	// One approval is decided once.
	if _, err := apps.Reject(ctx, scopeOf(admin), id.Ref(a.ID), api.RejectRequest{Reason: "late"}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("second decision: %v", err)
	}
	audit, _, _ := h.store.Audit().List(ctx, repo.AuditFilter{Action: "approval.approved"}, repo.Page{})
	if len(audit) != 1 || audit[0].Permission != domain.PermApprovalDecideClipSelection || audit[0].Actor != editor.Email {
		t.Fatalf("audit %+v", audit)
	}
	if countEvents(t, h.store, event.ApprovalDecided) != 1 {
		t.Fatal("approval.decided must fire")
	}

	// publication: an editor cannot; a publisher can (§15.6).
	h2 := newHarness(t)
	asset2 := h2.newAsset(domain.KindClip)
	h2.add(strings.Replace(gateDoc, "KIND", "publication", 1))
	j = h2.runAs(editor, "gate", map[string]any{"asset": asset2.ID})
	a = h2.pendingApproval(j)
	apps = h2.engine.Approvals()
	if _, err := apps.Approve(ctx, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("editor on publication: %v", err)
	}
	if _, err := apps.Reject(ctx, scopeOf(editor), id.Ref(a.ID), api.RejectRequest{Reason: "no"}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("editor rejecting a publication: %v", err)
	}
	if _, err := apps.Approve(ctx, scopeOf(publisher), id.Ref(a.ID), api.ApproveRequest{}); err != nil {
		t.Fatal(err)
	}
	h2.waitStatus(j, domain.JobCompleted)
}

func TestApproversTighten(t *testing.T) {
	h := newHarness(t)
	asset := h.newAsset(domain.KindOriginal)
	h.add(`{"id":"tight","version":"1.0.0","inputs":{"asset":{"type":"string"}},"nodes":[
		{"id":"ask","type":"approval.request","typeVersion":1,"parameters":{"kind":"clip_selection","assetId":"${inputs.asset}","approvers":["publisher"],"evidence":{"segments":[{"start_seconds":1,"end_seconds":2}]}}}]}`)
	j := h.run("tight", map[string]any{"asset": asset.ID})
	a := h.pendingApproval(j)
	ctx := context.Background()
	if _, err := h.engine.Approvals().Approve(ctx, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("an editor is not enough here: %v", err)
	}
	if _, err := h.engine.Approvals().Approve(ctx, scopeOf(publisher), id.Ref(a.ID), api.ApproveRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCompleted)
	// A list that loosens never gets past validation.
	res := h.val.Validate([]byte(`{"id":"loose","version":"1.0.0","nodes":[{"id":"ask","type":"approval.request","typeVersion":1,"parameters":{"kind":"publication","assetId":"ORI-001","approvers":["editor"]}}]}`))
	if res.OK() {
		t.Fatal("approvers must not loosen D7")
	}
}

func TestSelectionAdjustAmbiguousAndRejection(t *testing.T) {
	h := newHarness(t)
	asset := h.newAsset(domain.KindOriginal)
	h.add(strings.Replace(gateDoc, "KIND", "clip_selection", 1))
	h.add(`{"id":"ambig","version":"1.0.0","inputs":{"asset":{"type":"string"}},"nodes":[
		{"id":"ask","type":"approval.request","typeVersion":1,"parameters":{"kind":"clip_selection","assetId":"${inputs.asset}","allowAdjust":true,
			"evidence":{"segments":[{"start_seconds":1,"end_seconds":2}],"ambiguous":true,"candidates":[{"segments":[{"start_seconds":1,"end_seconds":2}],"excerpt":"a","confidence":0.4},{"segments":[{"start_seconds":5,"end_seconds":6}],"excerpt":"b","confidence":0.4}]}}}]}`)
	h.add(`{"id":"noadjust","version":"1.0.0","inputs":{"asset":{"type":"string"}},"nodes":[
		{"id":"ask","type":"approval.request","typeVersion":1,"parameters":{"kind":"clip_selection","assetId":"${inputs.asset}","evidence":{"segments":[{"start_seconds":1,"end_seconds":2}]}}}]}`)
	h.add(`{"id":"nothing","version":"1.0.0","inputs":{"asset":{"type":"string"}},"nodes":[
		{"id":"ask","type":"approval.request","typeVersion":1,"parameters":{"kind":"clip_selection","assetId":"${inputs.asset}"}}]}`)
	ctx := context.Background()
	apps := h.engine.Approvals()

	// Adjusting start and end, as `approval approve --start --end`.
	j := h.run("gate", map[string]any{"asset": asset.ID})
	a := h.pendingApproval(j)
	start, end := 12.5, 18.25
	if _, err := apps.Approve(ctx, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{StartSeconds: &start}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("start alone: %v", err)
	}
	bad := 1.0
	if _, err := apps.Approve(ctx, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{StartSeconds: &start, EndSeconds: &bad}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("end before start: %v", err)
	}
	if _, err := apps.Approve(ctx, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{StartSeconds: &start, EndSeconds: &end}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCompleted)
	var params struct {
		Segments []domain.Segment `json:"segments"`
	}
	if err := json.Unmarshal(h.callsOf("tail")[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	if len(params.Segments) != 1 || params.Segments[0].StartSeconds != 12.5 || params.Segments[0].EndSeconds != 18.25 || params.Segments[0].SourceAssetID != asset.ID || params.Segments[0].SourceVersion != "v1" {
		t.Fatalf("adjusted %+v", params.Segments)
	}

	// Ambiguous evidence needs a choice (§3.2, D51).
	j = h.run("ambig", map[string]any{"asset": asset.ID})
	a = h.pendingApproval(j)
	if _, err := apps.Approve(ctx, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := apps.Approve(ctx, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{Segments: []domain.Segment{{StartSeconds: 5, EndSeconds: 6}}}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCompleted)

	// Adjusting is ignored where the gate does not allow it.
	j = h.run("noadjust", map[string]any{"asset": asset.ID})
	a = h.pendingApproval(j)
	decided, err := apps.Approve(ctx, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{Segments: []domain.Segment{{StartSeconds: 100, EndSeconds: 200}}})
	if err != nil {
		t.Fatal(err)
	}
	if decided.Segments[0].StartSeconds != 1 {
		t.Fatalf("segments %+v", decided.Segments)
	}
	h.waitStatus(j, domain.JobCompleted)

	// A selection with nothing to select cannot be approved blind.
	j = h.run("nothing", map[string]any{"asset": asset.ID})
	a = h.pendingApproval(j)
	if _, err := apps.Approve(ctx, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("no selection: %v", err)
	}
	if _, err := apps.Reject(ctx, scopeOf(editor), id.Ref(a.ID), api.RejectRequest{Reason: "nothing useful"}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCompleted)

	// A rejection is a business outcome that flow.if routes (I41), and it has no segments.
	j = h.run("gate", map[string]any{"asset": asset.ID})
	a = h.pendingApproval(j)
	if _, err := apps.Reject(ctx, scopeOf(editor), id.Ref(a.ID), api.RejectRequest{Reason: "incomplete excerpt"}); err != nil {
		t.Fatal(err)
	}
	done := h.waitStatus(j, domain.JobCompleted)
	n := h.nodes(done)
	if n["tail"].Status != domain.NodeSkipped {
		t.Fatalf("tail %s", n["tail"].Status)
	}
	out := outputMap(t, n["ask"])
	if out["decision"] != "rejected" || out["reason"] != "incomplete excerpt" {
		t.Fatalf("output %v", out)
	}
	if _, has := out["segments"]; has {
		t.Fatalf("a rejected gate must have no segments: %v", out)
	}
}

func TestApprovalWaitNeedsNoClient(t *testing.T) {
	h := newHarness(t)
	asset := h.newAsset(domain.KindOriginal)
	h.add(strings.Replace(gateDoc, "KIND", "clip_selection", 1))
	j := h.run("gate", map[string]any{"asset": asset.ID})
	a := h.pendingApproval(j)
	// The daemon restarts while the job waits, which is what a disconnected CLI and a
	// lost process have in common: nothing was holding the wait.
	h.stopWorker()
	h.stopEngine()
	h.startEngine()
	h.startWorker()
	if got := h.job(j.ID).Status; got != domain.JobWaitingApproval {
		t.Fatalf("status %s", got)
	}
	if _, err := h.engine.Approvals().Approve(context.Background(), scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCompleted)
	// The same gate is not asked twice.
	list, _ := h.engine.Approvals().List(context.Background(), scopeOf(viewer), api.ApprovalQuery{})
	if len(list.Items) != 1 {
		t.Fatalf("approvals %+v", list.Items)
	}
}

func TestApprovalTimeoutPolicies(t *testing.T) {
	timeoutDoc := func(id, policy string) string {
		return `{"id":"` + id + `","version":"1.0.0","inputs":{"asset":{"type":"string"}},"nodes":[
			{"id":"ask","type":"approval.request","typeVersion":1,"parameters":{"kind":"clip_selection","assetId":"${inputs.asset}","timeoutSeconds":1,"onTimeout":"` + policy + `","evidence":{"segments":[{"start_seconds":1,"end_seconds":2}]}}},
			{"id":"tail","type":"test.pure","typeVersion":1,"parameters":{}}
		],"connections":{"ask":["tail"]}}`
	}
	ctx := context.Background()

	t.Run("fail", func(t *testing.T) {
		h := newHarness(t)
		asset := h.newAsset(domain.KindOriginal)
		h.add(timeoutDoc("t", "fail"))
		j := h.waitStatus(h.run("t", map[string]any{"asset": asset.ID}), domain.JobFailed)
		a, _, _ := h.store.Approvals().GetForNode(ctx, j.ULID, "ask")
		if a.Status != domain.ApprovalExpired || j.Error.Code != string(errs.CodeTimeout) {
			t.Fatalf("approval %s job error %+v", a.Status, j.Error)
		}
	})
	t.Run("reject", func(t *testing.T) {
		h := newHarness(t)
		asset := h.newAsset(domain.KindOriginal)
		h.add(timeoutDoc("t", "reject"))
		j := h.waitStatus(h.run("t", map[string]any{"asset": asset.ID}), domain.JobCompleted)
		a, _, _ := h.store.Approvals().GetForNode(ctx, j.ULID, "ask")
		if a.Status != domain.ApprovalRejected || a.DecidedBy != "system" {
			t.Fatalf("approval %+v", a)
		}
	})
	t.Run("remind", func(t *testing.T) {
		h := newHarness(t)
		asset := h.newAsset(domain.KindOriginal)
		h.add(timeoutDoc("t", "remind"))
		j := h.run("t", map[string]any{"asset": asset.ID})
		h.waitFor("a reminder", func() bool { return countEvents(t, h.store, event.ApprovalRequested) >= 2 })
		if st := h.job(j.ID).Status; st != domain.JobWaitingApproval {
			t.Fatalf("a reminder keeps it waiting: %s", st)
		}
		a, _, _ := h.store.Approvals().GetForNode(ctx, j.ULID, "ask")
		if _, err := h.engine.Approvals().Approve(ctx, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{}); err != nil {
			t.Fatal(err)
		}
		h.waitStatus(j, domain.JobCompleted)
	})
}

func TestCancelExpiresPendingApprovals(t *testing.T) {
	h := newHarness(t)
	asset := h.newAsset(domain.KindOriginal)
	h.add(strings.Replace(gateDoc, "KIND", "clip_selection", 1))
	j := h.run("gate", map[string]any{"asset": asset.ID})
	a := h.pendingApproval(j)
	ctx := context.Background()
	if _, err := h.engine.Jobs().Cancel(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCancelled)
	if _, err := h.engine.Approvals().Approve(ctx, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("a gate no job waits for cannot be decided: %v", err)
	}
	got, _ := h.engine.Approvals().Get(ctx, scopeOf(viewer), id.Ref(a.ID))
	if got.Status != domain.ApprovalExpired {
		t.Fatalf("approval %s", got.Status)
	}
}

func TestFlowWaitNeedsNoProcess(t *testing.T) {
	h := newHarness(t)
	h.add(`{"id":"waits","version":"1.0.0","nodes":[
		{"id":"w","type":"flow.wait","typeVersion":1,"parameters":{"seconds":1}},
		{"id":"tail","type":"test.pure","typeVersion":1,"parameters":{}}
	],"connections":{"w":["tail"]}}`)
	j := h.run("waits", nil)
	h.nodeStatus(j, "w", domain.NodeWaiting)
	// It survives a restart: the deadline is in the database.
	h.stopEngine()
	h.startEngine()
	h.waitStatus(j, domain.JobCompleted)
	if at := h.nodes(h.job(j.ID))["w"]; at.FinishedAt == nil || at.FinishedAt.Before(j.CreatedAt.Add(500*time.Millisecond)) {
		t.Fatalf("finished too early: %+v", at)
	}
	h.add(`{"id":"badwait","version":"1.0.0","nodes":[{"id":"w","type":"flow.wait","typeVersion":1,"parameters":{}}]}`)
	failed := h.waitStatus(h.run("badwait", nil), domain.JobFailed)
	if failed.Error.Code != string(errs.CodeInvalidArgument) {
		t.Fatalf("error %+v", failed.Error)
	}
}

func TestReadsNeverChangeAJob(t *testing.T) {
	h := newHarness(t)
	asset := h.newAsset(domain.KindOriginal)
	h.add(strings.Replace(gateDoc, "KIND", "clip_selection", 1))
	j := h.run("gate", map[string]any{"asset": asset.ID})
	a := h.pendingApproval(j)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := h.engine.Jobs().Subscribe(ctx, scopeOf(viewer), id.Ref(j.ID), 0)
	if err != nil {
		t.Fatal(err)
	}
	sawApproval := false
	for !sawApproval {
		select {
		case ev := <-stream:
			if ev.Kind == domain.JobEventApproval {
				sawApproval = true
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no approval event on the stream")
		}
	}
	// Ctrl+C on the client only drops the stream (§5.5).
	cancel()
	for range stream {
	}
	if st := h.job(j.ID).Status; st != domain.JobWaitingApproval {
		t.Fatalf("status %s", st)
	}
	// Reattach and resume from a sequence; the stream ends with the job.
	ctx2 := context.Background()
	again, err := h.engine.Jobs().Subscribe(ctx2, scopeOf(viewer), id.Ref(j.ID), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Approvals().Approve(ctx2, scopeOf(editor), id.Ref(a.ID), api.ApproveRequest{}); err != nil {
		t.Fatal(err)
	}
	var last domain.JobEvent
	var seq int64
	for ev := range again {
		if ev.Seq <= seq {
			t.Fatalf("sequence went back: %d after %d", ev.Seq, seq)
		}
		seq = ev.Seq
		last = ev
	}
	var data map[string]any
	_ = json.Unmarshal(last.Data, &data)
	if data["status"] != "completed" {
		t.Fatalf("last event %+v", last)
	}
	// Two clients can follow the same job (§5.3): a closed job still replays.
	replay, err := h.engine.Jobs().Subscribe(ctx2, scopeOf(viewer), id.Ref(j.ID), seq-1)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for range replay {
		count++
	}
	if count != 1 {
		t.Fatalf("replayed %d", count)
	}
}
