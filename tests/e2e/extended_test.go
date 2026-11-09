//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// Paths beyond the §1.1 table, on the real stack. Each exists because a component tested
// it against fakes and nothing else had run it against the others.

// jobOf finds the newest job of a workflow that this asset started after a mark.
func (sc *scenario) jobOf(t *testing.T, c *client, workflow string) domain.Job {
	t.Helper()
	var jobs api.List[domain.Job]
	c.jsonOut(t, &jobs, "job", "list", "--workflow", workflow, "--limit", "1")
	if len(jobs.Items) == 0 {
		t.Fatalf("no job of %s", workflow)
	}
	return jobs.Items[0]
}

// ---- D19, I33, I282 ----

func (sc *scenario) stepBudget(t *testing.T) {
	sc.admin.ok(t, "budget", "set", "--monthly", "0", "--currency", "USD")
	var started api.RunWorkflowResponse
	sc.editor.jsonOut(t, &started, "clip", "create", "--source", "ORI-002", "--prompt", "conscious breathing")
	jobID := string(started.JobID)

	// ai.find_segment can call the LLM, so a tenant over budget stops there (I282).
	var job api.JobDetail
	waitFor(t, time.Minute, "the job to pause on the budget", func() (bool, string) {
		sc.editor.jsonOut(t, &job, "job", "status", jobID)
		return job.Status == domain.JobPaused && job.StatusReason == "budget_exceeded", nodeSummary(job)
	})
	if node := nodeOf(t, job, "find_story"); node.Status == domain.NodeCompleted {
		t.Errorf("ai.find_segment ran over budget: %+v", node.Status)
	}

	sc.admin.ok(t, "budget", "override", "--reason", "demonstration", "--ttl", "1h")
	sc.editor.ok(t, "job", "resume", jobID)
	sc.waitJob(t, sc.editor, jobID, domain.JobWaitingApproval)

	// A rejected selection renders nothing, ends the job, and takes the preview with it.
	selection := sc.pendingApproval(t, sc.editor, jobID)
	var approval domain.Approval
	sc.editor.jsonOut(t, &approval, "approval", "show", selection)
	sc.editor.ok(t, "approval", "reject", selection, "--reason", "not this one")
	done := sc.waitJob(t, sc.editor, jobID, domain.JobCompleted)
	if render := nodeOf(t, done, "render"); render.Status != domain.NodeSkipped {
		t.Errorf("a rejected selection reached the render: %s", render.Status)
	}
	var outputs struct {
		Decision string `json:"decision"`
	}
	mustUnmarshal(t, done.Outputs, &outputs)
	if outputs.Decision != "rejected" {
		t.Errorf("job outputs: %s", done.Outputs)
	}
	waitFor(t, 30*time.Second, "the rejected preview's removal", func() (bool, string) {
		return !sc.stack.Exists(t, "storage/proxy/"+tenant+"/"+strings.TrimPrefix(approval.Preview, "storage://proxy/")), approval.Preview
	})
	sc.admin.ok(t, "budget", "set", "--monthly", "1000", "--currency", "USD")
}

// ---- Failure paths: ingest_failed, D52, D11 ----

func (sc *scenario) stepFailurePaths(t *testing.T) {
	t.Run("a-file-that-is-not-video", func(t *testing.T) {
		bad := filepath.Join(sc.tmp, "not-video.mp4")
		if err := os.WriteFile(bad, []byte(strings.Repeat("this is not a video\n", 1000)), 0o644); err != nil {
			t.Fatal(err)
		}
		var started struct {
			Job *api.RunWorkflowResponse `json:"job"`
		}
		sc.editor.jsonOut(t, &started, "original", "import", bad, "--title", "Broken file")
		failed := sc.waitTerminal(t, sc.editor, string(started.Job.JobID))
		if failed.Status != domain.JobFailed {
			t.Fatalf("a text file ended %s: %s", failed.Status, nodeSummary(failed))
		}
		var asset domain.Asset
		sc.editor.jsonOut(t, &asset, "asset", "show", string(failed.AssetID))
		if asset.Status != domain.AssetIngestFailed {
			t.Errorf("asset after a failed ingest: %s", asset.Status)
		}
		t.Logf("%s", nodeSummary(failed))
	})

	t.Run("a-destination-that-rejects", func(t *testing.T) {
		var started api.RunWorkflowResponse
		sc.publisher.jsonOut(t, &started, "publish", sc.grandchild, "--to", "mock-reject")
		sc.approvePending(t, sc.publisher)
		waitFor(t, jobTimeout, "the rejection to be recorded", func() (bool, string) {
			for _, pub := range sc.publications(t, sc.publisher, sc.grandchild) {
				if pub.Destination == "mock-reject" {
					return pub.Status == domain.PubRejectedByDestination, string(pub.Status)
				}
			}
			return false, "no publication yet"
		})
	})

	t.Run("an-unknown-outcome-with-no-reconcile", func(t *testing.T) {
		var started api.RunWorkflowResponse
		sc.publisher.jsonOut(t, &started, "publish", sc.grandchild, "--to", "mock-unknown")
		sc.approvePending(t, sc.publisher)
		var stuck domain.Job
		waitFor(t, jobTimeout, "needs_attention", func() (bool, string) {
			stuck = sc.jobOf(t, sc.publisher, "publish-one")
			return stuck.Status == domain.JobNeedsAttention, string(stuck.Status)
		})
		var detail api.JobDetail
		sc.admin.jsonOut(t, &detail, "job", "status", string(stuck.ID))
		if detail.Attention == nil || detail.Attention.NodeID == "" {
			t.Fatalf("needs_attention with nothing to look at: %+v", detail.Attention)
		}
		// An editor may not resolve a publication node (I150).
		sc.editor.refused(t, "forbidden", "job", "resolve", string(stuck.ID), "--retry-node", detail.Attention.NodeID)
		sc.admin.ok(t, "job", "resolve", string(stuck.ID), "--retry-node", detail.Attention.NodeID, "--note", "checked the destination")
		sc.waitJob(t, sc.admin, string(stuck.ID), domain.JobCompleted)
		state := sc.mock(t, "mock-unknown")
		if state.Uploads != 1 {
			t.Errorf("resolving with the same key uploaded %d times", state.Uploads)
		}
	})
}

// ---- Pause, resume, cancel (§6.5, R22) ----

func (sc *scenario) stepJobControl(t *testing.T) {
	file := filepath.Join(sc.tmp, "waits.json")
	definition := `{
  "id": "waits", "name": "Two waits", "version": "1.0.0",
  "nodes": [
    {"id": "one", "type": "flow.wait", "typeVersion": 1, "parameters": {"seconds": 2}},
    {"id": "two", "type": "flow.wait", "typeVersion": 1, "parameters": {"seconds": 2}}
  ],
  "connections": {"one": ["two"]}
}`
	if err := os.WriteFile(file, []byte(definition), 0o644); err != nil {
		t.Fatal(err)
	}
	sc.admin.ok(t, "workflow", "validate", file)
	var dry api.RunWorkflowResponse
	sc.admin.jsonOut(t, &dry, "workflow", "run", file, "--dry-run")
	if dry.Validation == nil || !dry.Validation.Valid {
		t.Fatalf("dry run: %+v", dry.Validation)
	}

	var paused api.RunWorkflowResponse
	sc.admin.jsonOut(t, &paused, "workflow", "run", file, "--idempotency-key", "waits-pause")
	sc.admin.ok(t, "job", "pause", string(paused.JobID))
	sc.waitJob(t, sc.admin, string(paused.JobID), domain.JobPaused)
	sc.admin.ok(t, "job", "resume", string(paused.JobID))
	sc.waitJob(t, sc.admin, string(paused.JobID), domain.JobCompleted)

	var cancelled api.RunWorkflowResponse
	sc.admin.jsonOut(t, &cancelled, "workflow", "run", file, "--idempotency-key", "waits-cancel")
	sc.admin.ok(t, "job", "cancel", string(cancelled.JobID))
	sc.waitJob(t, sc.admin, string(cancelled.JobID), domain.JobCancelled)
}

// ---- The other workflows of §3.3 ----

func (sc *scenario) stepOtherWorkflows(t *testing.T) {
	t.Run("verify", func(t *testing.T) {
		var verified api.StorageVerifyResponse
		sc.admin.jsonOut(t, &verified, "storage", "verify", "master", "--asset", "ORI-001")
		if !verified.OK || verified.Checked == 0 {
			t.Fatalf("verify master: %+v", verified)
		}
		sc.admin.jsonOut(t, &verified, "storage", "verify", "archive", "--asset", "ORI-001")
		if !verified.OK || verified.Checked == 0 {
			t.Fatalf("verify archive: %+v", verified)
		}
	})

	t.Run("reindex", func(t *testing.T) {
		var run api.RunWorkflowResponse
		sc.admin.jsonOut(t, &run, "workflow", "run", "reindex-asset", "--input", "assetId=ORI-002", "--idempotency-key", "reindex")
		sc.waitJob(t, sc.admin, string(run.JobID), domain.JobCompleted)
		var result api.SearchResponse
		sc.viewer.jsonOut(t, &result, "original", "search", "conscious breathing", "--asset", "ORI-002")
		if len(result.Items) == 0 {
			t.Error("no hits after reindexing")
		}
	})

	t.Run("export-package", func(t *testing.T) {
		var run api.RunWorkflowResponse
		sc.admin.jsonOut(t, &run, "asset", "export", "ORI-002")
		sc.waitJob(t, sc.admin, string(run.JobID), domain.JobCompleted)
	})

	t.Run("restore-archived", func(t *testing.T) {
		before := countArchiveReads(sc.accessEntries(t))
		// Without a terminal and without --yes it refuses (I171).
		if _, _, code := sc.admin.try(t, "storage", "restore", "--asset", "ORI-002", "--to", "master"); code == 0 {
			t.Fatal("a restore ran with no confirmation")
		}
		var run struct {
			Job api.RunWorkflowResponse `json:"job"`
		}
		sc.admin.jsonOut(t, &run, "storage", "restore", "--asset", "ORI-002", "--to", "master", "--yes")
		sc.waitJob(t, sc.admin, string(run.Job.JobID), domain.JobCompleted)
		// The one flow that may read archive, explicitly authorized, and it did.
		if after := countArchiveReads(sc.accessEntries(t)); after <= before {
			t.Errorf("restore-archived read nothing from archive: %d then %d", before, after)
		}
	})

	t.Run("bulk-create-clips", func(t *testing.T) {
		var run api.RunWorkflowResponse
		sc.editor.jsonOut(t, &run, "workflow", "run", "bulk-create-clips",
			"--input", "source_id=ORI-002", "--input", `prompts=["conscious breathing","the victorious breath"]`, "--idempotency-key", "bulk")
		seen := map[string]bool{}
		waitFor(t, jobTimeout, "both selections", func() (bool, string) {
			var approvals api.List[domain.Approval]
			sc.editor.jsonOut(t, &approvals, "approval", "list", "--status", "pending", "--kind", "clip_selection")
			for _, approval := range approvals.Items {
				if !seen[string(approval.ID)] {
					seen[string(approval.ID)] = true
					sc.editor.ok(t, "approval", "approve", string(approval.ID))
				}
			}
			return len(seen) == 2, fmt.Sprint(seen)
		})
		sc.waitJob(t, sc.editor, string(run.JobID), domain.JobCompleted)
	})

	t.Run("delete-asset", func(t *testing.T) {
		if _, _, code := sc.admin.try(t, "asset", "delete", sc.parentClip); code == 0 {
			t.Fatal("a delete ran with no confirmation")
		}
		var run api.RunWorkflowResponse
		sc.admin.jsonOut(t, &run, "asset", "delete", sc.parentClip, "--yes")
		sc.waitJob(t, sc.admin, string(run.JobID), domain.JobCompleted)
		sc.admin.refused(t, "not_found", "asset", "show", sc.parentClip)
		// Its clip stays, and the archive keeps its copy for the deferred purge (D17).
		var grandchild domain.Asset
		sc.admin.jsonOut(t, &grandchild, "asset", "show", sc.grandchild)
		if grandchild.Status != domain.AssetPublished {
			t.Errorf("a clip of the deleted clip is %s", grandchild.Status)
		}
	})

	t.Run("the-scheduled-workflows-run-on-demand", func(t *testing.T) {
		for _, workflow := range []string{"verify-storage", "expire-unapproved-renders"} {
			var run api.RunWorkflowResponse
			sc.admin.jsonOut(t, &run, "workflow", "run", workflow, "--idempotency-key", "on-demand-"+workflow)
			sc.waitJob(t, sc.admin, string(run.JobID), domain.JobCompleted)
		}
	})
}

func countArchiveReads(entries []accessEntry) int {
	count := 0
	for _, e := range entries {
		if e.Role == "archive" && e.Class == "read" {
			count++
		}
	}
	return count
}

// ---- §10.3, I128 ----

func (sc *scenario) stepMetrics(t *testing.T) {
	url := sc.stack.HTTPSURL() + "/api/v1/metrics"
	token := "Bearer " + sc.admin.credential(t)
	status, body := httpCall(t, sc.httpClient, http.MethodGet, url, map[string]string{"Authorization": token}, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /metrics: %d %s", status, truncate(string(body), 300))
	}
	text := string(body)
	for _, want := range []string{
		"vidyom_node_duration_seconds",
		`vidyom_connector_duration_seconds_count{driver="mock",family="publishing"`,
		`vidyom_connector_duration_seconds_count{driver="openai-compatible",family="llm"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %s in the metrics", want)
		}
	}
	// A tenant's view holds only that tenant's samples (I128).
	status, body = httpCall(t, sc.httpClient, http.MethodGet, url, map[string]string{"Authorization": token, api.HeaderTenant: "globex"}, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /metrics for a tenant: %d %s", status, truncate(string(body), 300))
	}
	if strings.Contains(string(body), `tenant_id="acme"`) {
		t.Error("globex's metrics show acme's samples")
	}
	// A viewer does not read metrics.
	status, _ = httpCall(t, sc.httpClient, http.MethodGet, url, map[string]string{"Authorization": "Bearer " + sc.viewer.credential(t), api.HeaderTenant: tenant}, nil)
	if status != http.StatusForbidden {
		t.Errorf("a viewer's GET /metrics: %d", status)
	}

	if address := sc.stack.WorkerMetricsURL(); address != "" {
		code, scraped, err := plainGet(address)
		if err != nil || code != http.StatusOK {
			t.Fatalf("worker metrics: %d %v", code, err)
		}
		for _, want := range []string{"vidyom_worker_", `family="storage"`, `vidyom_storage_transfer_bytes_total`} {
			if !strings.Contains(string(scraped), want) {
				t.Errorf("no %s in the worker's metrics", want)
			}
		}
	}
}

// ---- I17: same-host peer identity ----

func (sc *scenario) stepUnixSocket(t *testing.T) {
	socket := sc.stack.UnixSocket()
	if socket == "" {
		t.Skip("this stack has no socket on the host")
	}
	local := sc.newClient(t, "local")
	local.ok(t, "server", "add", "here", "--socket", socket)
	var assets api.List[domain.Asset]
	local.jsonOut(t, &assets, "--tenant", tenant, "asset", "list")
	if len(assets.Items) == 0 {
		t.Fatal("the peer identity found no assets")
	}
}

// ---- D38: the optional observability profile, Compose only ----

func (sc *scenario) stepObservabilityProfile(t *testing.T) {
	compose, ok := sc.stack.(*composeStack)
	if !ok {
		t.Skip("the observability profile is part of Compose")
	}
	// The daemon's stderr sink is what `docker compose logs` reads.
	if logs := compose.compose(t, "logs", "--no-color", "--tail", "20", "daemon"); !strings.Contains(logs, "http.request") {
		t.Errorf("docker compose logs daemon shows no requests: %s", truncate(logs, 500))
	}
	compose.startObservability(t)
	sc.admin.ok(t, "observability", "enable", "loki")
	sc.admin.ok(t, "observability", "enable", "tempo")

	small := sc.generateFixture(t, "small.mp4", 40, "640x360", 25)
	var started struct {
		Job *api.RunWorkflowResponse `json:"job"`
	}
	sc.editor.jsonOut(t, &started, "original", "import", small, "--title", "Observed class", "--type", "yoga_class", "--idempotency-key", "observed")
	job := sc.waitJob(t, sc.editor, string(started.Job.JobID), domain.JobCompleted)

	waitFor(t, 2*time.Minute, "logs in Loki", func() (bool, string) {
		code, body, err := plainGet(compose.lokiURL() + "/loki/api/v1/query_range?query=" + urlQuery(`{service="vidyomd"}`) + "&limit=5")
		if err != nil {
			return false, err.Error()
		}
		var out struct {
			Data struct {
				Result []json.RawMessage `json:"result"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &out)
		return code == http.StatusOK && len(out.Data.Result) > 0, fmt.Sprintf("%d %s", code, truncate(string(body), 300))
	})
	waitFor(t, 2*time.Minute, "the job's trace in Tempo", func() (bool, string) {
		code, body, err := plainGet(compose.tempoURL() + "/api/traces/" + job.TraceID)
		if err != nil {
			return false, err.Error()
		}
		return code == http.StatusOK && strings.Contains(string(body), "node "), fmt.Sprintf("%d %s", code, truncate(string(body), 200))
	})
	code, body, err := plainGet(compose.grafanaURL() + "/api/datasources")
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || !strings.Contains(string(body), "Loki") || !strings.Contains(string(body), "Tempo") {
		t.Errorf("Grafana's data sources: %d %s", code, truncate(string(body), 300))
	}
	t.Logf("Grafana lists: %s", truncate(string(body), 200))
	sc.admin.ok(t, "observability", "disable", "loki")
	sc.admin.ok(t, "observability", "disable", "tempo")
}
