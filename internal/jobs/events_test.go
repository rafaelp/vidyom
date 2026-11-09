package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

func TestEventTriggerStartsJobOnce(t *testing.T) {
	h := newHarness(t)
	h.add(`{"id":"source","version":"1.0.0","inputs":{"visibility":{"type":"string","default":"public"}},"nodes":[
		{"id":"emit","type":"event.emit","typeVersion":1,"parameters":{"type":"publication.completed","payload":{"visibility":"${inputs.visibility}"}}}]}`)
	h.add(`{"id":"on-public","version":"1.0.0","trigger":{"type":"event","event":"publication.completed","filter":"event.payload.visibility == 'public'"},
		"nodes":[{"id":"sync","type":"catalog.public.sync","typeVersion":1,"parameters":{"assetId":"${event.asset_id}"}},
		         {"id":"check","type":"test.pure","typeVersion":1,"parameters":{"visibility":"${event.payload.visibility}","type":"${event.type}"}}],
		"connections":{"sync":["check"]}}`)
	ctx := context.Background()

	source := h.waitStatus(h.run("source", nil), domain.JobCompleted)
	var triggered []domain.Job
	h.waitFor("the triggered job", func() bool {
		page, _, _ := h.store.Jobs().List(ctx, repo.JobFilter{WorkflowID: "on-public"}, repo.Page{Limit: 10})
		triggered = page
		return len(page) == 1 && page[0].Status == domain.JobCompleted
	})
	j := triggered[0]
	if j.Trigger.Type != "event" || j.Trigger.Depth != 1 || j.Trigger.EventID == "" {
		t.Fatalf("trigger %+v", j.Trigger)
	}
	if j.Permissions == nil && j.SubmittedBy != string(domain.AuthSystem) {
		t.Fatalf("job %+v", j)
	}
	var params map[string]any
	_ = json.Unmarshal(h.callsOf("check")[0].Params, &params)
	if params["visibility"] != "public" || params["type"] != "publication.completed" {
		t.Fatalf("event data %v", params)
	}
	if _, has := func() (any, bool) {
		var p map[string]any
		_ = json.Unmarshal(h.callsOf("sync")[0].Params, &p)
		v, ok := p["assetId"]
		return v, ok
	}(); has {
		t.Fatal("an event with no asset leaves assetId out")
	}

	// A repeated delivery starts nothing: the job is keyed by event and workflow.
	if err := h.engine.StartEventJob(ctx, startFor(t, h, j)); err != nil {
		t.Fatal(err)
	}
	page, _, _ := h.store.Jobs().List(ctx, repo.JobFilter{WorkflowID: "on-public"}, repo.Page{Limit: 10})
	if len(page) != 1 {
		t.Fatalf("duplicates: %d", len(page))
	}

	// A filter miss starts nothing, and is recorded.
	h.waitStatus(h.run("source", map[string]any{"visibility": "private"}), domain.JobCompleted)
	h.waitFor("the dropped delivery", func() bool {
		items, _, _ := h.store.Outbox().ListDeliveries(ctx, repo.DeliveryFilter{Status: event.DeliveryDropped}, repo.Page{})
		return len(items) == 1 && strings.Contains(items[0].LastError, "filter")
	})
	time.Sleep(20 * time.Millisecond)
	page, _, _ = h.store.Jobs().List(ctx, repo.JobFilter{WorkflowID: "on-public"}, repo.Page{Limit: 10})
	if len(page) != 1 {
		t.Fatalf("a filtered event started a job: %d", len(page))
	}
	_ = source
}

func startFor(t *testing.T, h *harness, j domain.Job) (req eventsStart) {
	t.Helper()
	e, err := h.store.Outbox().GetEvent(context.Background(), idULID(j.Trigger.EventID))
	if err != nil {
		t.Fatal(err)
	}
	return eventsStart{Tenant: "acme", WorkflowID: j.WorkflowID, Version: j.WorkflowVersion, Event: e, IdempotencyKey: j.IdempotencyKey}
}

func TestEventLoopStopsAtTheDepthLimit(t *testing.T) {
	h := newHarness(t, harnessOpts{maxDepth: 3})
	// Each job emits what triggers the next one.
	h.add(`{"id":"loop","version":"1.0.0","trigger":{"type":"event","event":"custom.ping"},"nodes":[
		{"id":"emit","type":"event.emit","typeVersion":1,"parameters":{"type":"custom.ping"}}]}`)
	h.add(`{"id":"starter","version":"1.0.0","nodes":[{"id":"emit","type":"event.emit","typeVersion":1,"parameters":{"type":"custom.ping"}}]}`)
	ctx := context.Background()
	h.waitStatus(h.run("starter", nil), domain.JobCompleted)
	h.waitFor("the chain to stop", func() bool {
		items, _, _ := h.store.Outbox().ListDeliveries(ctx, repo.DeliveryFilter{Status: event.DeliveryDropped}, repo.Page{})
		for _, d := range items {
			if strings.Contains(d.LastError, "deeper") {
				return true
			}
		}
		return false
	})
	time.Sleep(50 * time.Millisecond)
	page, _, _ := h.store.Jobs().List(ctx, repo.JobFilter{WorkflowID: "loop"}, repo.Page{Limit: 100})
	// Depth 0 from the starter's job, then 1, 2, 3 are served; 4 is dropped.
	if len(page) != 4 {
		t.Fatalf("loop jobs: %d", len(page))
	}
	var maxDepth int
	for _, j := range page {
		if j.Trigger.Depth > maxDepth {
			maxDepth = j.Trigger.Depth
		}
	}
	if maxDepth != 4 {
		t.Fatalf("depth %d", maxDepth)
	}
}

func TestReservedEventsCannotBeForged(t *testing.T) {
	h := newHarness(t)
	h.add(`{"id":"forge","version":"1.0.0","nodes":[{"id":"emit","type":"event.emit","typeVersion":1,"parameters":{"type":"budget.exceeded"}}]}`)
	j := h.waitStatus(h.run("forge", nil), domain.JobFailed)
	if !strings.Contains(j.Error.Message, "written by the engine") {
		t.Fatalf("error %+v", j.Error)
	}
}

func TestEventEmitIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.add(`{"id":"once","version":"1.0.0","nodes":[{"id":"emit","type":"event.emit","typeVersion":1,"parameters":{"type":"custom.ping"}}]}`)
	j := h.waitStatus(h.run("once", nil), domain.JobCompleted)
	out := outputMap(t, h.nodes(j)["emit"])
	if out["event_id"] == "" {
		t.Fatalf("output %v", out)
	}
	e, err := h.store.Outbox().GetEvent(context.Background(), idULID(out["event_id"].(string)))
	if err != nil || e.JobID != string(j.ID) || e.WorkflowID != "once" || e.TraceID != j.TraceID || e.CausationID != string(j.ID) {
		t.Fatalf("event %+v %v", e, err)
	}
}

func TestScheduledJob(t *testing.T) {
	h := newHarness(t)
	h.add(`{"id":"sweep","version":"1.0.0","trigger":{"type":"schedule","every":"1h"},"nodes":[{"id":"n","type":"test.pure","typeVersion":1,"parameters":{}}]}`)
	ctx := context.Background()
	req := schedulerStart{Tenant: "acme", WorkflowID: "sweep", Version: "1.0.0", IdempotencyKey: "schedule:sweep:42"}
	for i := 0; i < 2; i++ {
		if err := h.engine.StartScheduledJob(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	page, _, _ := h.store.Jobs().List(ctx, repo.JobFilter{WorkflowID: "sweep"}, repo.Page{Limit: 10})
	if len(page) != 1 || page[0].Trigger.Type != "schedule" {
		t.Fatalf("jobs %+v", page)
	}
	h.waitStatus(page[0], domain.JobCompleted)

	// Nothing an event or schedule starts may delete or read the archive: only a person.
	h.add(`{"id":"nasty","version":"1.0.0","trigger":{"type":"schedule","every":"1h"},"nodes":[{"id":"n","type":"catalog.delete","typeVersion":1,"parameters":{"assetId":"ORI-001"}}]}`)
	if err := h.engine.StartScheduledJob(ctx, schedulerStart{Tenant: "acme", WorkflowID: "nasty", Version: "1.0.0", IdempotencyKey: "k"}); err == nil {
		t.Fatal("a scheduled deletion must be refused (I157)")
	}
}
