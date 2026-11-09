package cli_test

import (
	"bufio"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/api"
)

// commandCase is one command, what it must send, and nothing else about its output:
// its human form only has to run, and its --json form has to parse.
type commandCase struct {
	args  []string
	route string // the mutating route, or the last route that must be hit
	query map[string]string
	body  map[string]any
	stdin string
	// stream marks NDJSON output under --json.
	stream bool
	// setup runs before the command.
	setup func(h *harness)
}

func args(line string) []string { return strings.Fields(line) }

func commandCases(t *testing.T) []commandCase {
	workflowFile := fileWith(t, "wf.json", []byte(`{"id":"local-flow","inputs":{"count":{"type":"number"}}}`))
	return []commandCase{
		{args: args("version"), route: api.RouteVersion},
		{args: args("daemon status"), route: api.RouteHealth},
		{args: args("daemon health"), route: api.RouteHealth},

		{args: args("device pairing-code create --ttl 15m --user maria@example.com"), route: api.RoutePairingCodes,
			body: map[string]any{"user_email": "maria@example.com", "ttl_seconds": 900.0}},
		{args: args("device list"), route: api.RouteDevicesList},
		{args: args("device revoke CLIENT-002"), route: api.RouteDevicesRevoke},
		{args: args("device rotate CLIENT-002"), route: api.RouteDevicesRotate},

		{args: args("tenant list"), route: api.RouteTenantsList},
		{args: args("tenant create beta --name Beta"), route: api.RouteTenantsAdd, body: map[string]any{"id": "beta", "name": "Beta"}},
		{args: args("user add maria@example.com --role editor"), route: api.RouteUsersAdd, body: map[string]any{"email": "maria@example.com", "role": "editor"}},
		{args: args("user list"), route: api.RouteUsersList},
		{args: args("user grant maria@example.com --permission storage.archive.restore"), route: api.RouteUsersGrant, body: map[string]any{"permission": "storage.archive.restore"}},

		{args: args("budget show"), route: api.RouteBudgetGet},
		{args: args("budget set --monthly 500 --currency USD"), route: api.RouteBudgetPut, body: map[string]any{"monthly_minor": 50000.0, "currency": "USD"}},
		{args: []string{"budget", "override", "--reason", "end-of-month release", "--ttl", "24h"}, route: api.RouteBudgetOver, body: map[string]any{"reason": "end-of-month release", "ttl_seconds": 86400.0}},

		{args: args("original list --status ready_for_clipping"), route: api.RouteAssetsList, query: map[string]string{"kind": "original", "status": "ready_for_clipping"}},
		{args: args("original show ORI-001"), route: api.RouteAssetsGet},
		{args: []string{"original", "search", "conscious breathing", "--asset", "ORI-001", "--mode", "lexical", "--limit", "5"}, route: api.RouteSearch,
			query: map[string]string{"q": "conscious breathing", "asset_id": "ORI-001", "mode": "lexical", "limit": "5"}},

		{args: args("asset list --kind clip --source ORI-001 --collection yoga"), route: api.RouteAssetsList, query: map[string]string{"kind": "clip", "source_id": "ORI-001", "collection": "yoga"}},
		{args: args("asset show CLIP-001"), route: api.RouteAssetsGet},
		{args: args("asset update ORI-001 --title New --tags a,b"), route: api.RouteAssetsUpdate, body: map[string]any{"title": "New", "tags": []any{"a", "b"}}},
		{args: args("asset lineage CLIP-001"), route: api.RouteAssetsLineage},
		{args: args("asset versions CLIP-001"), route: api.RouteAssetsVersions},
		{args: args("asset delete CLIP-001 --yes"), route: api.RouteAssetsDelete},
		{args: args("asset set-ai ORI-001 local_only"), route: api.RouteAssetsAIProcessing, body: map[string]any{"ai_processing": "local_only"}},
		{args: args("asset consent ORI-001"), route: api.RouteAssetsConsent},
		{args: args("asset transcript ORI-001 --version v2"), route: api.RouteAssetsTranscript, query: map[string]string{"version": "v2"}},
		{args: args("asset proxy-link ORI-001"), route: api.RouteAssetsProxyLink},
		{args: args("asset export ORI-001"), route: api.RouteAssetsExport},
		{args: args("asset sensitive list ORI-001"), route: api.RouteSensitiveList},
		{args: args("asset sensitive add ORI-001 --start 00:00:10 --end 00:00:20 --reason minors"), route: api.RouteSensitiveAdd,
			body: map[string]any{"start_seconds": 10.0, "end_seconds": 20.0, "reason": "minors"}},
		{args: args("asset sensitive remove ORI-001 --start 10 --end 20"), route: api.RouteSensitiveRemove, body: map[string]any{"start_seconds": 10.0, "end_seconds": 20.0}},

		{args: args("collection list"), route: api.RouteCollectionsList},
		{args: args("collection set-ai --name yoga external_allowed"), route: api.RouteCollectionsAIProcessing, body: map[string]any{"ai_processing": "external_allowed"}},

		{args: []string{"clip", "create", "--source", "ORI-001", "--prompt", "The story of the 90-year-old woman", "--workflow", "create-clip"}, route: api.RouteClipsCreate,
			body: map[string]any{"source_id": "ORI-001", "prompt": "The story of the 90-year-old woman"}},
		{args: args("clip preview CLIP-001"), route: api.RouteAssetsProxyLink},
		{args: args("clip list --source ORI-001"), route: api.RouteAssetsList, query: map[string]string{"kind": "clip", "source_id": "ORI-001"}},

		{args: args("approval list --status pending --kind publication --asset CLIP-001"), route: api.RouteApprovalsList,
			query: map[string]string{"status": "pending", "kind": "publication", "asset_id": "CLIP-001"}},
		{args: args("approval show APR-001"), route: api.RouteApprovalsGet},
		{args: args("approval approve APR-001 --start 00:10:48 --end 00:18:00"), route: api.RouteApprovalsApprove, body: map[string]any{"start_seconds": 648.0, "end_seconds": 1080.0}},
		{args: []string{"approval", "reject", "APR-001", "--reason", "incomplete excerpt"}, route: api.RouteApprovalsReject, body: map[string]any{"reason": "incomplete excerpt"}},

		{args: args("workflow list"), route: api.RouteWorkflowsList},
		{args: args("workflow show ingest-original"), route: api.RouteWorkflowsGet},
		{args: []string{"workflow", "validate", workflowFile}, route: api.RouteWorkflowsValidate},
		{args: []string{"workflow", "run", workflowFile, "--input", "count=3"}, route: api.RouteWorkflowsRunBody, body: map[string]any{"inputs": map[string]any{"count": 3.0}}},
		{args: []string{"workflow", "run", "ingest-original", "--input", "title=A", "--input", "file=/data/class.mp4"}, route: api.RouteWorkflowsRun,
			body: map[string]any{"inputs": map[string]any{"title": "A", "file": "server:/data/class.mp4"}}},

		{args: args("node list"), route: api.RouteNodesList},
		{args: args("node describe video.cut"), route: api.RouteNodesGet},
		{args: args("node run ai.transcribe --asset ORI-001 --param language=pt"), route: api.RouteNodesRun,
			body: map[string]any{"asset_id": "ORI-001", "parameters": map[string]any{"language": "pt"}}},

		{args: args("job list --status needs_attention --workflow create-clip --asset ORI-001"), route: api.RouteJobsList,
			query: map[string]string{"status": "needs_attention", "workflow": "create-clip", "asset_id": "ORI-001"}},
		{args: args("job status JOB-001"), route: api.RouteJobsGet},
		{args: args("job attach JOB-001"), route: api.RouteJobsEvents, stream: true},
		{args: args("job logs JOB-001"), route: api.RouteJobsLogs},
		{args: args("job logs JOB-001 --follow"), route: api.RouteJobsEvents, stream: true},
		{args: args("job retry JOB-001"), route: api.RouteJobsRetry},
		{args: args("job pause JOB-001"), route: api.RouteJobsPause},
		{args: args("job resume JOB-001"), route: api.RouteJobsResume},
		{args: args("job cancel JOB-001"), route: api.RouteJobsCancel},
		{args: args("job resolve JOB-001 --retry-node publish"), route: api.RouteJobsResolve, body: map[string]any{"action": "retry_node", "node_id": "publish"}},

		{args: args("connector list"), route: api.RouteConnectors},
		{args: args("storage list"), route: api.RouteStoragesList},
		{args: args("storage test master"), route: api.RouteStoragesTest},
		{args: args("storage verify archive --asset ORI-001"), route: api.RouteStoragesVerify, body: map[string]any{"asset_id": "ORI-001"}},
		{args: args("storage copy --from master --to proxy --asset ORI-001"), route: api.RouteStoragesCopy, body: map[string]any{"from": "master", "to": "proxy", "asset_id": "ORI-001"}},
		{args: args("storage restore --asset ORI-001 --to master --yes"), route: api.RouteStoragesRestore, body: map[string]any{"asset_id": "ORI-001", "to": "master", "confirm": true}},

		{args: args("publishing list"), route: api.RouteDestinationsList},
		{args: args("publish CLIP-001 --to youtube-main,vimeo-main"), route: api.RoutePublicationsCreate, body: map[string]any{"asset_id": "CLIP-001", "destinations": []any{"youtube-main", "vimeo-main"}}},
		{args: args("publish status CLIP-001"), route: api.RoutePublicationsList, query: map[string]string{"asset_id": "CLIP-001"}},
		{args: args("publish status PUB-001"), route: api.RoutePublicationsGet},
		{args: args("publish sync CLIP-001"), route: api.RoutePublicationsSync},
		{args: args("unpublish CLIP-001 --all"), route: api.RoutePublicationsUnpub, body: map[string]any{"asset_id": "CLIP-001", "all": true}},
		{args: args("unpublish CLIP-001 --from youtube-main"), route: api.RoutePublicationsUnpub, body: map[string]any{"asset_id": "CLIP-001", "destinations": []any{"youtube-main"}}},
		{args: args("unpublish PUB-001"), route: api.RoutePublicationsWithdraw},

		{args: args("secret add gcs-main --location server"), route: api.RouteSecretsPut, stdin: "fictional-value\n", body: map[string]any{"name": "gcs-main", "location": "server"}},
		{args: args("secret list"), route: api.RouteSecretsList},
		{args: args("secret test gcs-main"), route: api.RouteSecretsTest},
		{args: args("secret rotate gcs-main"), route: api.RouteSecretsRotate, stdin: "fictional-new-value"},
		{args: args("secret delete gcs-main"), route: api.RouteSecretsDelete},

		{args: args("security status"), route: api.RouteDoctor, query: map[string]string{"check": "security"}},
		{args: args("security audit --since 24h --actor admin@example.com"), route: api.RouteAuditList, query: map[string]string{"since": "2026-11-08T12:00:00Z", "actor": "admin@example.com"}},
		{args: args("security audit --verify"), route: api.RouteAuditVerify},
		{args: args("security audit --export"), route: api.RouteAuditExport},
		{args: args("security sessions"), route: api.RouteDevicesList},
		{args: args("security revoke CLIENT-002"), route: api.RouteDevicesRevoke},

		{args: args("logs --since 24h --level warn --job JOB-001"), route: api.RouteLogsList, query: map[string]string{"level": "warn", "job_id": "JOB-001"}},
		{args: args("logs --follow"), route: api.RouteLogsStream, stream: true},
		{args: args("observability list"), route: api.RouteObsList},
		{args: args("observability test loki"), route: api.RouteObsTest},
		{args: args("observability enable loki"), route: api.RouteObsEnable},
		{args: args("observability disable loki"), route: api.RouteObsDisable},
		{args: args("observability status"), route: api.RouteObsStatus},
		{args: args("doctor --security"), route: api.RouteDoctor, query: map[string]string{"check": "security"}},
		{args: args("doctor --network"), route: api.RouteDoctor, query: map[string]string{"check": "network"}},
		{args: args("doctor --storage"), route: api.RouteDoctor, query: map[string]string{"check": "storage"}},
		{args: args("doctor"), route: api.RouteDoctor},

		{args: args("plugin list"), route: api.RoutePluginsList},
		{args: args("plugin inspect notifications"), route: api.RoutePluginsGet},
		{args: args("plugin install /srv/plugins/notifications"), route: api.RoutePluginsAdd, body: map[string]any{"path": "/srv/plugins/notifications"}},
		{args: args("hook list --workflow create-clip"), route: api.RouteHooksList, query: map[string]string{"workflow": "create-clip"}},
		{args: args("hook test validate-source --asset ORI-001"), route: api.RouteHooksTest, body: map[string]any{"asset_id": "ORI-001"}},
		{args: args("event list --since 24h --type clip.created"), route: api.RouteEventsList, query: map[string]string{"type": "clip.created", "since": "2026-11-08T12:00:00Z"}},
		{args: args("event watch --type clip.created"), route: api.RouteEventsStream, query: map[string]string{"type": "clip.created"}, stream: true},
		{args: args("event deliveries --status failed"), route: api.RouteEventsDeliv, query: map[string]string{"status": "failed"}},
	}
}

// excludedRoutes have no command, on purpose (I178).
var excludedRoutes = map[string]string{
	api.RouteMetrics:       "scrape target with controlled access, not a client command",
	api.RouteClientsList:   "alias of /devices; the CLI uses /devices (I38)",
	api.RouteClientsRevoke: "alias of /devices; the CLI uses /devices (I38)",
}

func checkCall(t *testing.T, h *harness, tc commandCase) {
	t.Helper()
	call, ok := h.srv.Last(tc.route)
	if !ok {
		t.Fatalf("route %s was not hit; calls: %v", tc.route, h.srv.Calls())
	}
	for key, want := range tc.query {
		if got := call.Query.Get(key); got != want {
			t.Errorf("query %s = %q, want %q (%s)", key, got, want, describe(call))
		}
	}
	if tc.body != nil {
		body := call.JSONBody(t)
		for key, want := range tc.body {
			if !reflect.DeepEqual(body[key], want) {
				t.Errorf("body %s = %#v, want %#v (%s)", key, body[key], want, describe(call))
			}
		}
	}
}

func TestEveryCommandInHumanAndJSONForm(t *testing.T) {
	cases := commandCases(t)
	hit := map[string]bool{}
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.srv.EnableUploads(1 << 16)

	for _, tc := range cases {
		name := strings.Join(tc.args, " ")
		t.Run(name, func(t *testing.T) {
			h.t = t
			h.srv.Reset()
			h.stdin = tc.stdin
			if tc.setup != nil {
				tc.setup(h)
			}
			if code := h.run(tc.args...); code != 0 {
				t.Fatalf("human form: exit %d\nstdout: %s\nstderr: %s", code, h.stdout.String(), h.stderr.String())
			}
			if h.stdout.Len() == 0 {
				t.Error("human form printed nothing")
			}
			checkCall(t, h, tc)
			for name := range h.srv.Hit() {
				hit[name] = true
			}

			h.srv.Reset()
			if code := h.run(append(append([]string{}, tc.args...), "--json")...); code != 0 {
				t.Fatalf("JSON form: exit %d\nstdout: %s\nstderr: %s", code, h.stdout.String(), h.stderr.String())
			}
			checkCall(t, h, tc)
			assertJSONOutput(t, h.stdout.String(), tc.stream)
		})
	}
	h.t = t

	// Commands with their own scenario in other tests reach these routes.
	for _, route := range []string{api.RouteDevicesPair, api.RouteUploadsCreate, api.RouteUploadsGet, api.RouteUploadsChunk, api.RouteUploadsComplete, api.RouteUploadsAbort} {
		hit[route] = true
	}
	var missing []string
	for _, route := range api.Routes {
		if route.Auth == api.AuthWorker || excludedRoutes[route.Name] != "" || hit[route.Name] {
			continue
		}
		missing = append(missing, route.Name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("routes with no command: %v", missing)
	}
}

func assertJSONOutput(t *testing.T, stdout string, stream bool) {
	t.Helper()
	if strings.TrimSpace(stdout) == "" {
		t.Fatal("--json printed nothing")
	}
	if !stream {
		var value any
		if err := json.Unmarshal([]byte(stdout), &value); err != nil {
			t.Fatalf("--json is not one JSON document: %v\n%s", err, stdout)
		}
		return
	}
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		var value map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			t.Fatalf("a stream line is not JSON: %v\n%s", err, scanner.Text())
		}
	}
}

// A body must hold only contract fields: the server decodes strictly (I125).
func TestBodiesDecodeStrictlyAgainstTheContract(t *testing.T) {
	strict := map[string]func() any{
		api.RouteBudgetPut:          func() any { return &api.SetBudgetRequest{} },
		api.RouteApprovalsApprove:   func() any { return &api.ApproveRequest{} },
		api.RouteJobsResolve:        func() any { return &api.ResolveJobRequest{} },
		api.RouteWorkflowsRun:       func() any { return &api.RunWorkflowRequest{} },
		api.RouteClipsCreate:        func() any { return &api.CreateClipRequest{} },
		api.RoutePublicationsCreate: func() any { return &api.CreatePublicationRequest{} },
		api.RouteStoragesRestore:    func() any { return &api.StorageRestoreRequest{} },
		api.RouteSecretsPut:         func() any { return &api.PutSecretRequest{} },
		api.RouteSensitiveAdd:       func() any { return &api.SensitiveSegmentRequest{} },
		api.RouteUsersAdd:           func() any { return &api.AddUserRequest{} },
	}
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.srv.EnableUploads(1 << 16)
	seen := map[string]bool{}
	for _, tc := range commandCases(t) {
		newBody, ok := strict[tc.route]
		if !ok || seen[tc.route] {
			continue
		}
		seen[tc.route] = true
		h.srv.Reset()
		h.stdin = tc.stdin
		h.mustRun(tc.args...)
		call, _ := h.srv.Last(tc.route)
		decoder := json.NewDecoder(strings.NewReader(string(call.Body)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(newBody()); err != nil {
			t.Errorf("%s: %v in %s", tc.route, err, call.Body)
		}
	}
	if len(seen) != len(strict) {
		t.Errorf("checked %d of %d routes", len(seen), len(strict))
	}
}
