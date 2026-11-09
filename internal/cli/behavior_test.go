package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/apiclient/apitest"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

const d29Statement = "Image and voice authorizations from speakers and participants are the tenant's responsibility"

// ---- Exit codes and error output ----

func TestExitCodes(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()

	if code := h.run("job", "status"); code != 2 {
		t.Errorf("missing argument: exit %d", code)
	}
	if code := h.run("nonsense"); code != 2 {
		t.Errorf("unknown command: exit %d", code)
	}
	if code := h.run("job", "nonsense"); code != 2 {
		t.Errorf("unknown subcommand: exit %d, stdout %q", code, h.stdout.String())
	}
	if code := h.run("job", "list", "--nonsense"); code != 2 {
		t.Errorf("unknown flag: exit %d", code)
	}
	if code := h.run("job", "status", "not-an-id"); code != 2 {
		t.Errorf("invalid ID: exit %d", code)
	}
	if code := h.run(); code != 0 {
		t.Errorf("no arguments prints help: exit %d", code)
	}
	if code := h.run("job"); code != 0 {
		t.Errorf("a group prints help: exit %d", code)
	}
	if code := h.run("--help"); code != 0 {
		t.Errorf("--help: exit %d", code)
	}

	h.srv.Error(api.RouteAssetsGet, errs.NotFound("asset"))
	if code := h.run("asset", "show", "ORI-404"); code != 1 {
		t.Errorf("API error: exit %d", code)
	}
	assertContains(t, h.stderr.String(), "asset not found", "req-test")
}

func TestJSONErrorsGoToStderrAsTheAPIFormat(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.srv.Error(api.RouteApprovalsGet, errs.Forbidden("approval.decide.publication"))
	if code := h.run("approval", "show", "APR-002", "--json"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if h.stdout.Len() != 0 {
		t.Errorf("stdout must stay empty on error: %s", h.stdout.String())
	}
	body := decodeJSON(t, h.stderr.String())
	failure, _ := body["error"].(map[string]any)
	if failure["code"] != "forbidden" || failure["request_id"] != "req-test" {
		t.Fatalf("error = %v", body)
	}
}

func TestNotFoundSaysWhichTenantWasSearched(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.srv.Error(api.RouteAssetsGet, errs.NotFound("asset"))
	if code := h.run("asset", "show", "ORI-404"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "tenant acme")
}

func TestInterruptedPromptExitsAsInterrupted(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.terminal = true
	h.password = func() ([]byte, error) { return nil, errors.New("read interrupted") }
	ctx, interrupt := context.WithCancel(context.Background())
	interrupt()
	if code := h.runContext(ctx, "secret", "add", "x", "--location", "server"); code != 130 {
		t.Fatalf("exit %d, stderr %s", code, h.stderr.String())
	}
	if len(h.srv.Mutations()) != 0 {
		t.Fatal("nothing may be sent")
	}
}

func TestUsageErrorsInJSON(t *testing.T) {
	h := newHarness(t)
	if code := h.run("job", "status", "--json"); code != 2 {
		t.Fatalf("exit %d", code)
	}
	body := decodeJSON(t, h.stderr.String())
	if failure, _ := body["error"].(map[string]any); failure["code"] != "invalid_argument" {
		t.Fatalf("error = %v", body)
	}
}

func TestTenantRequiredHint(t *testing.T) {
	h := newHarness(t)
	h.mustRun("server", "add", "test", "--socket", h.socket)
	if code := h.run("job", "list"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "no tenant selected", "vidyom tenant use")
	if len(h.srv.Calls()) != 0 {
		t.Fatal("nothing may be sent without the tenant")
	}
}

func TestServerProvidedTextCannotDriveTheTerminal(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	asset := sampleAsset()
	asset.Title = "bad\x1b[2Jtitle\x07"
	h.srv.JSON(api.RouteAssetsList, 200, api.List[domain.Asset]{Items: []domain.Asset{asset}})
	h.mustRun("asset", "list")
	if strings.ContainsAny(h.stdout.String(), "\x1b\x07") {
		t.Fatalf("control characters reached the terminal: %q", h.stdout.String())
	}
	assertContains(t, h.stdout.String(), "bad[2Jtitle")
}

// ---- Tenant selection (D32, §11.1) ----

func TestTenantUseAndOverride(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()

	h.mustRun("job", "list")
	call, _ := h.srv.Last(api.RouteJobsList)
	if call.Header.Get(api.HeaderTenant) != "acme" {
		t.Errorf("tenant = %q", call.Header.Get(api.HeaderTenant))
	}

	h.mustRun("job", "list", "--tenant", "beta")
	call, _ = h.srv.Last(api.RouteJobsList)
	if call.Header.Get(api.HeaderTenant) != "beta" {
		t.Errorf("--tenant must override: %q", call.Header.Get(api.HeaderTenant))
	}

	// An empty --tenant= drops the header on optional routes (I121).
	h.mustRun("device", "list", "--tenant=")
	call, _ = h.srv.Last(api.RouteDevicesList)
	if call.Header.Get(api.HeaderTenant) != "" {
		t.Errorf("--tenant= must drop the header: %q", call.Header.Get(api.HeaderTenant))
	}

	if code := h.run("job", "list", "--tenant", "../etc"); code != 2 {
		t.Errorf("a bad slug must be a usage error: exit %d", code)
	}
}

func TestTenantUseRefusesATenantYouDoNotHave(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	if code := h.run("tenant", "use", "other"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(h.config(), `"tenant": "other"`) {
		t.Fatal("the refused tenant was stored")
	}
}

func TestTenantUseWorksBeforePairing(t *testing.T) {
	h := newHarness(t)
	h.mustRun("server", "add", "test", "--socket", h.socket)
	h.srv.Error(api.RouteTenantsList, errs.New(errs.CodeUnauthenticated, "no credential"))
	h.mustRun("tenant", "use", "acme")
	assertContains(t, h.stderr.String(), "could not check")
	assertContains(t, h.config(), `"tenant": "acme"`)
}

// ---- Implicit local server (I162) ----

func TestImplicitLocalServerUsesTheSocket(t *testing.T) {
	h := newHarness(t)
	h.srv.JSON(api.RouteHealth, 200, api.HealthResponse{Status: "ok"})
	h.mustRun("daemon", "health")
	if _, err := os.Stat(filepath.Join(h.dir, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reading health must not write a configuration: %v", err)
	}
	assertContains(t, h.mustRun("server", "list"), "local", h.socket)
}

func TestUnreachableDaemonIsClear(t *testing.T) {
	h := newHarness(t)
	h.env["VIDYOM_SOCKET"] = filepath.Join(h.dir, "absent.sock")
	if code := h.run("daemon", "health"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "cannot reach the daemon", "absent.sock", "hint:")
}

func TestUnhealthyDaemonShowsWhyAndExitsNonZero(t *testing.T) {
	h := newHarness(t)
	h.srv.JSON(api.RouteHealth, http.StatusServiceUnavailable, api.HealthResponse{
		Status: "down", Components: map[string]api.ComponentHealth{"redis": {Status: "down"}},
	})
	if code := h.run("daemon", "health"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stdout.String(), "redis", "down")
}

// ---- Approvals (D7, D29, R40) ----

func TestApprovalShowOnPublicationStatesTenantResponsibility(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.srv.JSON(api.RouteApprovalsGet, 200, sampleApproval(domain.ApprovalPublication))

	assertContains(t, h.mustRun("approval", "show", "APR-002"), d29Statement, "does not track or enforce")

	out := decodeJSON(t, h.mustRun("approval", "show", "APR-002", "--json"))
	notices, _ := out["notices"].([]any)
	if len(notices) != 1 || !strings.Contains(notices[0].(string), d29Statement) {
		t.Fatalf("--json must carry the statement: %v", out)
	}
	if out["id"] != "APR-001" || out["kind"] != "publication" {
		t.Fatalf("the approval's own fields must stay at the top level: %v", out)
	}
}

func TestApprovalShowOnSelectionHasNoNotice(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.srv.JSON(api.RouteApprovalsGet, 200, sampleApproval(domain.ApprovalClipSelection))
	if out := h.mustRun("approval", "show", "APR-001"); strings.Contains(out, d29Statement) {
		t.Fatalf("a selection approval is not the publication screen: %s", out)
	}
	assertContains(t, h.mustRun("approval", "show", "APR-001"), "the story", "0.9")
}

func TestApprovalApproveOnPublicationStatesResponsibility(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.srv.JSON(api.RouteApprovalsGet, 200, sampleApproval(domain.ApprovalPublication))
	h.srv.JSON(api.RouteApprovalsApprove, 200, sampleApproval(domain.ApprovalPublication))
	h.mustRun("approval", "approve", "APR-002")
	assertContains(t, h.stderr.String(), d29Statement)
}

func TestApprovalApproveForbiddenIsReported(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.srv.JSON(api.RouteApprovalsGet, 200, sampleApproval(domain.ApprovalPublication))
	h.srv.Error(api.RouteApprovalsApprove, errs.Forbidden("approval.decide.publication"))
	if code := h.run("approval", "approve", "APR-002", "--json"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	body := decodeJSON(t, h.stderr.String())
	if failure, _ := body["error"].(map[string]any); failure["code"] != "forbidden" {
		t.Fatalf("error = %v", body)
	}
}

func TestApproveRefusesAdjustingWhatCannotBeAdjusted(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	approval := sampleApproval(domain.ApprovalClipSelection)
	approval.AllowAdjust = false
	h.srv.JSON(api.RouteApprovalsGet, 200, approval)
	if code := h.run("approval", "approve", "APR-001", "--start", "00:00:10", "--end", "00:00:20"); code != 2 {
		t.Fatalf("exit %d, stderr %s", code, h.stderr.String())
	}
	if len(h.srv.Mutations()) != 0 {
		t.Fatal("nothing may be decided: the server would ignore the adjustment silently")
	}
}

func TestApproveIntervalValidation(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.srv.JSON(api.RouteApprovalsGet, 200, sampleApproval(domain.ApprovalClipSelection))
	h.srv.JSON(api.RouteApprovalsApprove, 200, sampleApproval(domain.ApprovalClipSelection))
	bad := [][]string{
		{"--start", "00:00:10"},
		{"--end", "00:00:10"},
		{"--start", "00:00:20", "--end", "00:00:10"},
		{"--start", "garbage", "--end", "00:00:10"},
		{"--start", "10", "--end", "20", "--segment", "1-2"},
		{"--segment", "20-10"},
		{"--segment", "only-one-side-"},
	}
	for _, flags := range bad {
		if code := h.run(append([]string{"approval", "approve", "APR-001"}, flags...)...); code != 2 {
			t.Errorf("%v: exit %d", flags, code)
		}
	}
	if len(h.srv.Mutations()) != 0 {
		t.Fatal("no decision may be sent")
	}

	h.mustRun("approval", "approve", "APR-001", "--segment", "00:01:00-00:02:00", "--segment", "180.5-200")
	call, _ := h.srv.Last(api.RouteApprovalsApprove)
	var req api.ApproveRequest
	if err := json.Unmarshal(call.Body, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Segments) != 2 || req.Segments[0].StartSeconds != 60 || req.Segments[1].StartSeconds != 180.5 ||
		req.Segments[0].SourceAssetID != "ORI-001" || req.Segments[0].SourceVersion != "v1" {
		t.Fatalf("segments = %+v", req.Segments)
	}
}

func TestClipApproveShortcut(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	pub := sampleApproval(domain.ApprovalPublication)
	h.srv.JSON(api.RouteApprovalsApprove, 200, pub)

	h.srv.JSON(api.RouteApprovalsList, 200, api.List[domain.Approval]{Items: []domain.Approval{}})
	if code := h.run("clip", "approve", "CLIP-001"); code != 1 {
		t.Fatalf("no pending: exit %d", code)
	}
	assertContains(t, h.stderr.String(), "no pending publication approval", "approval approve APR-nnn")

	second := pub
	second.ID = "APR-009"
	h.srv.JSON(api.RouteApprovalsList, 200, api.List[domain.Approval]{Items: []domain.Approval{pub, second}})
	if code := h.run("clip", "approve", "CLIP-001"); code != 1 {
		t.Fatalf("several pending: exit %d", code)
	}
	assertContains(t, h.stderr.String(), "APR-001", "APR-009")
	if len(h.srv.Mutations()) != 0 {
		t.Fatal("it must never guess among several")
	}

	h.srv.JSON(api.RouteApprovalsList, 200, api.List[domain.Approval]{Items: []domain.Approval{pub}})
	h.mustRun("clip", "approve", "CLIP-001")
	assertContains(t, h.stderr.String(), d29Statement)
	call, _ := h.srv.Last(api.RouteApprovalsList)
	if call.Query.Get("asset_id") != "CLIP-001" || call.Query.Get("kind") != "publication" || call.Query.Get("status") != "pending" {
		t.Fatalf("query = %v", call.Query)
	}
	if decided, ok := h.srv.Last(api.RouteApprovalsApprove); !ok || !strings.HasSuffix(decided.Path, "/APR-001/approve") {
		t.Fatalf("decided = %+v", decided)
	}
}

// ---- Jobs ----

func TestAttachExitCodeFollowsTheOutcome(t *testing.T) {
	cases := []struct {
		status domain.JobStatus
		want   int
	}{
		{domain.JobCompleted, 0},
		{domain.JobFailed, 1},
		{domain.JobCancelled, 1},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			h := newHarness(t)
			h.useTenant("acme")
			h.installSamples()
			job := sampleJob()
			job.Status = tc.status
			if tc.status == domain.JobFailed {
				job.Error = &domain.JobError{Code: "ffmpeg", Message: "exit 1"}
			}
			h.srv.JSON(api.RouteJobsGet, 200, api.JobDetail{Job: job})
			if code := h.run("job", "attach", "JOB-001"); code != tc.want {
				t.Fatalf("exit %d, want %d\n%s", code, tc.want, h.stderr.String())
			}
			assertContains(t, h.stdout.String(), string(tc.status))
			if len(h.srv.Mutations()) != 0 {
				t.Fatalf("attach changed something: %v", h.srv.Mutations())
			}
		})
	}
}

func TestAttachJSONEndsWithTheOutcome(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	job := sampleJob()
	job.Status = domain.JobCompleted
	h.srv.JSON(api.RouteJobsGet, 200, api.JobDetail{Job: job})
	h.mustRun("job", "attach", "JOB-001", "--json")
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %v", lines)
	}
	first, last := decodeJSON(t, lines[0]), decodeJSON(t, lines[2])
	if first["seq"] != 1.0 || first["message"] != "line one" {
		t.Errorf("first = %v", first)
	}
	if last["end"] != true || last["status"] != "completed" {
		t.Errorf("last = %v", last)
	}
}

// Ctrl+C on attach and logs --follow must only disconnect (§5.5, §11.5).
func TestInterruptingAStreamNeverTouchesTheJob(t *testing.T) {
	for _, command := range [][]string{{"job", "attach", "JOB-001"}, {"job", "logs", "JOB-001", "--follow"}} {
		t.Run(command[1], func(t *testing.T) {
			h := newHarness(t)
			h.useTenant("acme")
			h.installSamples()
			connected := make(chan struct{})
			disconnected := make(chan struct{})
			h.srv.Handle(api.RouteJobsEvents, func(w http.ResponseWriter, r *http.Request) {
				apitest.WriteSSEHeaders(w)
				apitest.WriteSSE(w, apitest.SSEFrame{ID: "1", Name: api.SSEJobEvent, Data: domain.JobEvent{Seq: 1, JobID: "JOB-001", At: sampleTime, Kind: domain.JobEventLog, Message: "running"}})
				close(connected)
				<-r.Context().Done()
				close(disconnected)
			})
			ctx, interrupt := context.WithCancel(context.Background())
			defer interrupt()
			go func() {
				<-connected
				interrupt()
			}()
			code := h.runContext(ctx, command...)
			if code != 130 {
				t.Fatalf("exit %d, stderr %s", code, h.stderr.String())
			}
			assertContains(t, h.stderr.String(), "keeps running")
			select {
			case <-disconnected:
			case <-time.After(5 * time.Second):
				t.Fatal("the connection stayed open")
			}
			if mutations := h.srv.Mutations(); len(mutations) != 0 {
				t.Fatalf("disconnecting changed the job: %v", mutations)
			}
		})
	}
}

func TestLogsFollowOnlyShowsLogs(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	out := h.mustRun("job", "logs", "JOB-001", "--follow")
	assertContains(t, out, "line one")
	if strings.Contains(out, "running") {
		t.Fatalf("state events must not appear in logs: %s", out)
	}
}

func TestAttachReconnectsWithLastEventID(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	var resumed int64 = -2
	connection := 0
	h.srv.Handle(api.RouteJobsEvents, func(w http.ResponseWriter, r *http.Request) {
		apitest.WriteSSEHeaders(w)
		connection++
		if connection == 1 {
			apitest.WriteSSE(w, apitest.SSEFrame{ID: "5", Name: api.SSEJobEvent, Data: domain.JobEvent{Seq: 5, JobID: "JOB-001", At: sampleTime, Kind: domain.JobEventLog, Message: "before restart"}})
			return
		}
		resumed = apitest.LastEventID(r)
		apitest.WriteSSE(w, apitest.SSEFrame{Name: api.SSEEnd})
	})
	h.mustRun("job", "attach", "JOB-001")
	if resumed != 5 {
		t.Fatalf("resumed from %d, want 5", resumed)
	}
	assertContains(t, h.stderr.String(), "reconnecting")
}

func TestEventWatchAndLogsFollowCanBeInterrupted(t *testing.T) {
	cases := []struct {
		route   string
		command []string
	}{
		{api.RouteEventsStream, []string{"event", "watch"}},
		{api.RouteLogsStream, []string{"logs", "--follow"}},
	}
	for _, tc := range cases {
		t.Run(tc.route, func(t *testing.T) {
			h := newHarness(t)
			h.useTenant("acme")
			connected := make(chan struct{})
			h.srv.Handle(tc.route, func(w http.ResponseWriter, r *http.Request) {
				apitest.WriteSSEHeaders(w)
				close(connected)
				<-r.Context().Done()
			})
			ctx, interrupt := context.WithCancel(context.Background())
			defer interrupt()
			go func() {
				<-connected
				interrupt()
			}()
			if code := h.runContext(ctx, tc.command...); code != 130 {
				t.Fatalf("exit %d, stderr %s", code, h.stderr.String())
			}
			if len(h.srv.Mutations()) != 0 {
				t.Fatal("following must not mutate")
			}
		})
	}
}

func TestJobResolveNeedsExactlyOneAction(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	for _, flags := range [][]string{{}, {"--retry-node", "a", "--fail"}, {"--retry-node", "a", "--skip-node", "b"}} {
		if code := h.run(append([]string{"job", "resolve", "JOB-001"}, flags...)...); code != 2 {
			t.Errorf("%v: exit %d", flags, code)
		}
	}
	if len(h.srv.Mutations()) != 0 {
		t.Fatal("nothing may be sent")
	}
	h.mustRun("job", "resolve", "JOB-001", "--fail", "--note", "checked remote")
	call, _ := h.srv.Last(api.RouteJobsResolve)
	body := call.JSONBody(t)
	if body["action"] != "fail" || body["note"] != "checked remote" {
		t.Fatalf("body = %v", body)
	}
	h.mustRun("job", "resolve", "JOB-001", "--skip-node", "publish")
	call, _ = h.srv.Last(api.RouteJobsResolve)
	if body := call.JSONBody(t); body["action"] != "skip_node" || body["node_id"] != "publish" {
		t.Fatalf("body = %v", body)
	}
}

func TestJobStatusShowsAttention(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	job := sampleJob()
	job.Status = domain.JobNeedsAttention
	job.Attention = &domain.AttentionInfo{NodeID: "publish", NodeType: "publish.upload", Reason: "outcome unknown", OutcomeUnknown: true, Since: sampleTime}
	h.srv.JSON(api.RouteJobsGet, 200, api.JobDetail{Job: job})
	assertContains(t, h.mustRun("job", "status", "JOB-001"), "needs_attention", "publish", "--retry-node publish")
}

// ---- Listing ----

func TestListPaginationFlags(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.srv.Handle(api.RouteJobsList, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "":
			apitest.WriteJSON(w, 200, api.List[domain.Job]{Items: []domain.Job{sampleJob()}, NextCursor: "c2"})
		default:
			apitest.WriteJSON(w, 200, api.List[domain.Job]{Items: []domain.Job{sampleJob()}})
		}
	})
	h.mustRun("job", "list", "--limit", "1")
	assertContains(t, h.stderr.String(), "--cursor c2")
	call, _ := h.srv.Last(api.RouteJobsList)
	if call.Query.Get("limit") != "1" {
		t.Errorf("limit = %q", call.Query.Get("limit"))
	}
	out := decodeJSON(t, h.mustRun("job", "list", "--all", "--json"))
	if items, _ := out["items"].([]any); len(items) != 2 {
		t.Fatalf("--all = %v", out)
	}
	out = decodeJSON(t, h.mustRun("job", "list", "--json"))
	if out["next_cursor"] != "c2" {
		t.Fatalf("next_cursor = %v", out["next_cursor"])
	}
}

func TestEmptyListIsAnArrayInJSON(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.srv.JSON(api.RouteJobsList, 200, map[string]any{})
	out := decodeJSON(t, h.mustRun("job", "list", "--json"))
	if items, ok := out["items"].([]any); !ok || len(items) != 0 {
		t.Fatalf("items = %#v", out["items"])
	}
}

// ---- Publishing ----

func TestUnpublishRules(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	for _, command := range [][]string{
		{"unpublish", "CLIP-001"},
		{"unpublish", "CLIP-001", "--all", "--from", "youtube-main"},
		{"unpublish", "PUB-001", "--all"},
	} {
		if code := h.run(command...); code != 2 {
			t.Errorf("%v: exit %d", command, code)
		}
	}
	if len(h.srv.Mutations()) != 0 {
		t.Fatal("nothing may be sent")
	}
	// Repeating is not an error: idempotent on the server (D17), and no prompt (immediate).
	h.mustRun("unpublish", "CLIP-001", "--all")
	h.mustRun("unpublish", "CLIP-001", "--all")
	if n := len(h.srv.Calls(api.RoutePublicationsUnpub)); n != 2 {
		t.Fatalf("calls = %d", n)
	}
}

func TestPublishNeedsDestination(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	if code := h.run("publish", "CLIP-001"); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestPublishSyncDerivesKeysFromAFixedOne(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	second := samplePublication()
	second.ID = "PUB-002"
	h.srv.JSON(api.RoutePublicationsList, 200, api.List[domain.Publication]{Items: []domain.Publication{samplePublication(), second}})
	h.mustRun("publish", "sync", "CLIP-001", "--idempotency-key", "fixed")
	calls := h.srv.Calls(api.RoutePublicationsSync)
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	one, two := calls[0].Header.Get(api.HeaderIdempotencyKey), calls[1].Header.Get(api.HeaderIdempotencyKey)
	if one == two || one == "fixed" {
		t.Fatalf("one key on two requests would be refused (I122): %q, %q", one, two)
	}
}

func TestFixedIdempotencyKeyReachesASingleMutation(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.mustRun("publish", "CLIP-001", "--to", "mock-main", "--idempotency-key", "fixed-key")
	call, _ := h.srv.Last(api.RoutePublicationsCreate)
	if call.Header.Get(api.HeaderIdempotencyKey) != "fixed-key" {
		t.Fatalf("key = %q", call.Header.Get(api.HeaderIdempotencyKey))
	}
}

// ---- Confirmations (I178) ----

func TestAssetDeleteAsksFirst(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()

	if code := h.run("asset", "delete", "CLIP-001"); code != 1 {
		t.Fatalf("no terminal and no --yes: exit %d", code)
	}
	assertContains(t, h.stderr.String(), "--yes")
	if len(h.srv.Mutations()) != 0 {
		t.Fatal("it must refuse without confirmation")
	}

	h.terminal = true
	h.stdin = "n\n"
	if code := h.run("asset", "delete", "CLIP-001"); code != 1 {
		t.Fatalf("answer no: exit %d", code)
	}
	if len(h.srv.Mutations()) != 0 {
		t.Fatal("no means no")
	}

	h.stdin = "yes\n"
	h.mustRun("asset", "delete", "CLIP-001")
	if len(h.srv.Calls(api.RouteAssetsDelete)) != 1 {
		t.Fatal("yes must delete")
	}
}

func TestStorageRestoreShowsCostBeforeConfirming(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()

	if code := h.run("storage", "restore", "--asset", "ORI-001", "--to", "master"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "12.50 USD", "1.0 GiB", "--yes")
	calls := h.srv.Calls(api.RouteStoragesRestore)
	if len(calls) != 1 || calls[0].JSONBody(t)["confirm"] != nil {
		t.Fatalf("only the preview may be sent: %v", calls)
	}

	h.srv.Reset()
	if code := h.run("storage", "restore", "--asset", "ORI-001", "--to", "master", "--json"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	preview := decodeJSON(t, h.stdout.String())
	if detail, _ := preview["preview"].(map[string]any); detail["archive_bytes"] != float64(1<<30) {
		t.Fatalf("a script must see the cost: %v", preview)
	}

	h.srv.Reset()
	h.terminal = true
	h.stdin = "y\n"
	assertContains(t, h.mustRun("storage", "restore", "--asset", "ORI-001", "--to", "master"), "JOB-001")
	calls = h.srv.Calls(api.RouteStoragesRestore)
	if len(calls) != 2 || calls[1].JSONBody(t)["confirm"] != true {
		t.Fatalf("calls = %v", calls)
	}
	if calls[0].Header.Get(api.HeaderIdempotencyKey) == calls[1].Header.Get(api.HeaderIdempotencyKey) {
		t.Fatal("the two phases are different requests and need different keys")
	}

	h.srv.Reset()
	h.terminal = false
	h.stdin = ""
	out := decodeJSON(t, h.mustRun("storage", "restore", "--asset", "ORI-001", "--to", "master", "--yes", "--json"))
	if out["preview"] == nil || out["job"] == nil {
		t.Fatalf("out = %v", out)
	}
}

func TestStorageRestoreWithoutCostTable(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.srv.Handle(api.RouteStoragesRestore, func(w http.ResponseWriter, r *http.Request) {
		var req api.StorageRestoreRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Confirm {
			apitest.WriteJSON(w, 200, api.StorageRestorePreview{ConfirmRequired: true, AssetID: "ORI-001", To: req.To, ArchiveBytes: 2048})
			return
		}
		apitest.WriteJSON(w, 202, sampleRun())
	})
	h.mustRun("storage", "restore", "--asset", "ORI-001", "--to", "master", "--yes")
	assertContains(t, h.stderr.String(), "not available", "2.0 KiB")
}

// ---- Exit codes on verification ----

func TestVerificationFailuresExitNonZero(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(h *harness)
		command []string
		want    string
	}{
		{"audit chain", func(h *harness) {
			h.srv.JSON(api.RouteAuditVerify, 200, domain.AuditVerification{OK: false, Records: 9, FirstBadSeq: 4, Reason: "hash mismatch"})
		}, []string{"security", "audit", "--verify"}, "BROKEN"},
		{"storage verify", func(h *harness) {
			h.srv.JSON(api.RouteStoragesVerify, 200, api.StorageVerifyResponse{OK: false, Checked: 2, Failed: []string{"master.mp4"}})
		}, []string{"storage", "verify", "master", "--asset", "ORI-001"}, "master.mp4"},
		{"storage test", func(h *harness) {
			h.srv.JSON(api.RouteStoragesTest, 200, api.StorageTestResponse{OK: false, Detail: "denied"})
		}, []string{"storage", "test", "master"}, "denied"},
		{"secret test", func(h *harness) {
			h.srv.JSON(api.RouteSecretsTest, 200, api.SecretTestResponse{OK: false, Detail: "expired"})
		}, []string{"secret", "test", "gcs-main"}, "expired"},
		{"doctor", func(h *harness) {
			h.srv.JSON(api.RouteDoctor, 200, api.DoctorResponse{Checks: []api.DoctorCheck{{Name: "redis", Status: "fail", Detail: "down"}}})
		}, []string{"doctor", "--network"}, "redis"},
		{"workflow validate", func(h *harness) {
			h.srv.JSON(api.RouteWorkflowsValidate, 200, api.ValidationResult{Valid: false, Issues: []api.ValidationIssue{{Severity: "error", Code: "cycle", Message: "a cycle"}}})
		}, []string{"workflow", "validate", "WF"}, "a cycle"},
		{"dry run", func(h *harness) {
			h.srv.JSON(api.RouteWorkflowsRun, 200, api.RunWorkflowResponse{Workflow: "w", WorkflowVersion: "1", Validation: &api.ValidationResult{Valid: false, Issues: []api.ValidationIssue{{Severity: "error", Code: "x", Message: "bad input"}}}})
		}, []string{"workflow", "run", "ingest-original", "--dry-run"}, "bad input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.useTenant("acme")
			h.installSamples()
			tc.setup(h)
			command := append([]string{}, tc.command...)
			for i, arg := range command {
				if arg == "WF" {
					command[i] = fileWith(t, "wf.json", []byte(`{"id":"x"}`))
				}
			}
			if code := h.run(command...); code != 1 {
				t.Fatalf("exit %d\n%s\n%s", code, h.stdout.String(), h.stderr.String())
			}
			assertContains(t, h.stdout.String(), tc.want)
		})
	}
}

func TestDryRunWarningIsShownAndSucceeds(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.srv.JSON(api.RouteWorkflowsRun, 200, api.RunWorkflowResponse{Workflow: "w", WorkflowVersion: "1", Validation: &api.ValidationResult{
		Valid: true, Issues: []api.ValidationIssue{{Severity: "warning", Code: "client_credential", Message: "uses a client credential"}},
	}})
	out := h.mustRun("workflow", "run", "ingest-original", "--dry-run")
	assertContains(t, out, "Nothing was started", "client credential")
	call, _ := h.srv.Last(api.RouteWorkflowsRun)
	if call.JSONBody(t)["dry_run"] != true {
		t.Fatalf("body = %s", call.Body)
	}
}

// ---- Doctor ----

func TestDoctorReportsAStoppedDaemonAndTheClient(t *testing.T) {
	h := newHarness(t)
	h.env["VIDYOM_SOCKET"] = filepath.Join(h.dir, "absent.sock")
	if code := h.run("doctor", "--security", "--json"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	out := decodeJSON(t, h.stdout.String())
	raw, _ := json.Marshal(out)
	assertContains(t, string(raw), "client.transport", "client.keyring", "client.daemon")
}

func TestDoctorKeyringChecksByTransport(t *testing.T) {
	h := newHarness(t)
	h.keyring.getErr = security.ErrKeyringUnavailable
	h.srv.JSON(api.RouteDoctor, 200, api.DoctorResponse{})
	if code := h.run("doctor", "--security"); code != 0 {
		t.Fatalf("a Unix socket does not need a keyring: exit %d\n%s", code, h.stdout.String())
	}
	assertContains(t, h.stdout.String(), "warn", "peer identity")
}

func TestDoctorPermissionErrorIsReported(t *testing.T) {
	h := newHarness(t)
	h.srv.Error(api.RouteDoctor, errs.Forbidden("doctor"))
	if code := h.run("doctor", "--security"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "doctor")
}

// ---- Plugin paths ----

func TestPluginInstallPathRules(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.mustRun("plugin", "install", "./notifications-plugin")
	call, _ := h.srv.Last(api.RoutePluginsAdd)
	path, _ := call.JSONBody(t)["path"].(string)
	if !filepath.IsAbs(path) || !strings.HasSuffix(path, "notifications-plugin") {
		t.Fatalf("on the same host a relative path resolves here: %q", path)
	}

	url, fp := h.srv.ServeTLS()
	h.mustRun("server", "add", "remote", "--url", url, "--fingerprint", fp)
	h.keyring.entries["vidyom/device/remote"] = "vdm_fictional"
	h.srv.Reset()
	if code := h.run("plugin", "install", "./notifications-plugin", "--server", "remote", "--tenant", "acme"); code != 2 {
		t.Fatalf("a remote server cannot take a relative path: exit %d", code)
	}
	if len(h.srv.Mutations()) != 0 {
		t.Fatal("nothing may be sent")
	}
}
