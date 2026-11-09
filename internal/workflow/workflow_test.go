package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

func hasIssue(res Result, code string) bool {
	for _, i := range res.Issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

func mustValid(t *testing.T, v *Validator, doc string) Result {
	t.Helper()
	res := v.Validate([]byte(doc))
	if !res.OK() {
		t.Fatalf("expected valid, got %+v", res.Issues)
	}
	return res
}

func TestParseTemplate(t *testing.T) {
	cases := []struct {
		in   string
		refs int
		bad  bool
		lit  string
	}{
		{in: "plain", lit: "plain"},
		{in: "${inputs.a}", refs: 1},
		{in: "a-${nodes.b-c.output.d_e}-f", refs: 1},
		{in: "${nodes.x.output}", refs: 1},
		{in: "${event.payload.visibility}", refs: 1},
		{in: "${job.id}", refs: 1},
		{in: "$${inputs.a}", lit: "${inputs.a}"},
		{in: "$${", lit: "${"},
		{in: "${inputs.a", bad: true},
		{in: "${nodes.x}", bad: true},
		{in: "${nodes.x.status}", bad: true},
		{in: "${inputs}", bad: true},
		{in: "${inputs.a + 1}", bad: true},
		{in: "${job.secret}", bad: true},
		{in: "${env.HOME}", bad: true},
		{in: "${}", bad: true},
	}
	for _, c := range cases {
		parts, err := ParseTemplate(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q: expected an error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		refs := 0
		var lit strings.Builder
		for _, p := range parts {
			if p.Ref != nil {
				refs++
			}
			lit.WriteString(p.Literal)
		}
		if refs != c.refs {
			t.Errorf("%q: %d refs, want %d", c.in, refs, c.refs)
		}
		if c.lit != "" && lit.String() != c.lit {
			t.Errorf("%q: literal %q, want %q", c.in, lit.String(), c.lit)
		}
	}
}

func testScope() Scope {
	return Scope{
		Inputs: map[string]any{"title": "Class", "n": json.Number("3"), "list": []any{"a", "b"}},
		Nodes: map[string]NodeData{
			"reg":     {Output: map[string]any{"id": "ORI-001", "items": []any{map[string]any{"k": "v"}}}, Status: "completed"},
			"skipped": {Status: "skipped"},
		},
		Job: map[string]any{"id": "JOB-001"},
	}
}

func TestResolveParameters(t *testing.T) {
	raw := json.RawMessage(`{
		"whole": "${nodes.reg.output}",
		"text": "originals/${nodes.reg.output.id}/${inputs.n}/x.mp4",
		"typed": "${inputs.n}",
		"item": "${nodes.reg.output.items.0.k}",
		"gone": "${nodes.skipped.output.id}",
		"missing": "${inputs.nothing}",
		"escaped": "$${inputs.title}",
		"nested": {"a": ["${inputs.title}"], "b": "${nodes.skipped.output}"},
		"job": "${job.id}",
		"condition": "keep ${this}"
	}`)
	out, err := ResolveParameters(raw, testScope(), map[string]bool{"condition": true})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["text"] != "originals/ORI-001/3/x.mp4" || got["typed"] != float64(3) || got["item"] != "v" {
		t.Fatalf("resolved %v", got)
	}
	if _, ok := got["gone"]; ok {
		t.Fatal("a reference to a skipped node must be absent")
	}
	if _, ok := got["missing"]; ok {
		t.Fatal("an input with no value must be absent")
	}
	if got["escaped"] != "${inputs.title}" {
		t.Fatalf("escape: %v", got["escaped"])
	}
	if got["condition"] != "keep ${this}" {
		t.Fatalf("a skipped key must stay as written: %v", got["condition"])
	}
	if got["job"] != "JOB-001" {
		t.Fatalf("job: %v", got["job"])
	}
	nested := got["nested"].(map[string]any)
	if _, ok := nested["b"]; ok {
		t.Fatal("nested absent key must go")
	}
	if got["whole"].(map[string]any)["id"] != "ORI-001" {
		t.Fatalf("whole: %v", got["whole"])
	}
}

func TestResolveFailures(t *testing.T) {
	for _, raw := range []string{
		`{"x": "a-${nodes.skipped.output.id}"}`,
		`{"x": ["${nodes.skipped.output.id}"]}`,
		`{"x": "a-${nodes.reg.output}"}`,
		`{"x": "${bad}"}`,
	} {
		if _, err := ResolveParameters(json.RawMessage(raw), testScope(), nil); err == nil {
			t.Errorf("%s: expected an error", raw)
		}
	}
}

func TestCondition(t *testing.T) {
	c, err := CompileCondition(`nodes.review.output.decision == 'approved' && inputs.visibility == 'public' && inputs.n > 2`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Nodes, ",") != "review" || strings.Join(c.Inputs, ",") != "n,visibility" {
		t.Fatalf("references %v %v", c.Nodes, c.Inputs)
	}
	scope := Scope{
		Inputs: map[string]any{"visibility": "public", "n": json.Number("3")},
		Nodes:  map[string]NodeData{"review": {Output: map[string]any{"decision": "approved"}, Status: "completed"}},
	}
	ok, err := c.Eval(context.Background(), scope)
	if err != nil || !ok {
		t.Fatalf("eval: %v %v", ok, err)
	}
	if _, err := CompileCondition(`nodes.a.output.x ==`); err == nil {
		t.Fatal("a syntax error must fail")
	}
	if _, err := CompileCondition(`unknown.x == 1`); err == nil {
		t.Fatal("an unknown variable must fail")
	}
	if _, err := CompileCondition(`1 + 2`); err == nil {
		t.Fatal("a non-boolean expression must fail")
	}
	if _, err := CompileFilter(`nodes.a.output.x == 1`); err == nil {
		t.Fatal("a filter sees only event")
	}
	idx, err := CompileCondition(`nodes["my-node"].output.ok`)
	if err != nil || strings.Join(idx.Nodes, ",") != "my-node" {
		t.Fatalf("indexed: %v %v", idx, err)
	}
	dyn, err := CompileCondition(`nodes[inputs.which].output.ok`)
	if err != nil || !dyn.DynamicNodes {
		t.Fatalf("dynamic: %v %v", dyn, err)
	}
	// A missing field is an evaluation error, which fails the node (I23).
	missing, err := CompileCondition(`nodes.a.output.nope == 1`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Eval(context.Background(), Scope{Nodes: map[string]NodeData{"a": {Output: map[string]any{}}}}); err == nil {
		t.Fatal("expected an evaluation error")
	}
}

func TestConditionCostLimit(t *testing.T) {
	c, err := CompileCondition(`inputs.list.all(a, inputs.list.all(b, inputs.list.all(c, inputs.list.all(d, a + b + c + d >= 0))))`)
	if err != nil {
		t.Fatal(err)
	}
	list := make([]any, 200)
	for i := range list {
		list[i] = json.Number("1")
	}
	start := time.Now()
	_, err = c.Eval(context.Background(), Scope{Inputs: map[string]any{"list": list}})
	if err == nil {
		t.Fatal("an expensive condition must hit the cost limit")
	}
	if !strings.Contains(err.Error(), "cost") && !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "interrupt") {
		t.Fatalf("unexpected error: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

func TestGraph(t *testing.T) {
	def := &wfdef.Definition{
		Nodes: []wfdef.NodeSpec{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}},
		Connections: map[string][]string{
			"a": {"b", "c"},
			"b": {"d"},
			"c": {"d"},
		},
	}
	g, problems := BuildGraph(def)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	if strings.Join(g.Order, "") != "abcd" {
		t.Fatalf("order %v", g.Order)
	}
	if !g.IsAncestor("a", "d") || g.IsAncestor("d", "a") || g.IsAncestor("b", "c") {
		t.Fatal("ancestors")
	}
	if !g.Dominates("a", "d") || g.Dominates("b", "d") {
		t.Fatal("dominators")
	}
	_, problems = BuildGraph(&wfdef.Definition{
		Nodes:       []wfdef.NodeSpec{{ID: "a"}, {ID: "b"}},
		Connections: map[string][]string{"a": {"b"}, "b": {"a"}},
	})
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), "cycle") {
		t.Fatalf("cycle: %v", problems)
	}
	_, problems = BuildGraph(&wfdef.Definition{
		Nodes:       []wfdef.NodeSpec{{ID: "a"}, {ID: "a"}},
		Connections: map[string][]string{"x": {"a"}, "a": {"y"}},
	})
	if len(problems) != 3 {
		t.Fatalf("problems: %v", problems)
	}
}

func TestValidatorRules(t *testing.T) {
	v := NewValidator(nil)
	base := func(nodes, connections string) string {
		return `{"id":"t","version":"1.0.0","inputs":{"a":{"type":"string"}},"nodes":[` + nodes + `],"connections":{` + connections + `}}`
	}
	cases := []struct {
		name string
		doc  string
		code string
	}{
		{"schema", `{"id":"t","nodes":[]}`, IssueSchema},
		{"unknown field", `{"id":"t","version":"1.0.0","nodes":[{"id":"x","type":"flow.if","typeVersion":1}],"foo":1}`, IssueSchema},
		{"unknown type", base(`{"id":"x","type":"not.a_node","typeVersion":1}`, ``), IssueNodeType},
		{"unknown version", base(`{"id":"x","type":"flow.if","typeVersion":9,"parameters":{"condition":"true"}}`, ``), IssueNodeType},
		{"cycle", base(`{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}},{"id":"y","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}`, `"x":["y"],"y":["x"]`), IssueGraph},
		{"missing required", base(`{"id":"x","type":"flow.if","typeVersion":1}`, ``), IssueParameters},
		{"wrong literal type", base(`{"id":"x","type":"catalog.get","typeVersion":1,"parameters":{"id":"ORI-001","requireClipEligible":"yes"}}`, ``), IssueParameters},
		{"unknown parameter", base(`{"id":"x","type":"catalog.get","typeVersion":1,"parameters":{"id":"ORI-001","nope":1}}`, ``), IssueParameters},
		{"bad reference", base(`{"id":"x","type":"catalog.get","typeVersion":1,"parameters":{"id":"${inputs}"}}`, ``), IssueReference},
		{"undeclared input", base(`{"id":"x","type":"catalog.get","typeVersion":1,"parameters":{"id":"${inputs.nope}"}}`, ``), IssueUnknownInput},
		{"not an ancestor", base(`{"id":"x","type":"catalog.get","typeVersion":1,"parameters":{"id":"ORI-001"}},{"id":"y","type":"catalog.get","typeVersion":1,"parameters":{"id":"${nodes.x.output.id}"}}`, ``), IssueNotAncestor},
		{"unknown node", base(`{"id":"x","type":"catalog.get","typeVersion":1,"parameters":{"id":"${nodes.z.output.id}"}}`, ``), IssueReference},
		{"event without trigger", base(`{"id":"x","type":"catalog.get","typeVersion":1,"parameters":{"id":"${event.asset_id}"}}`, ``), IssueEventReference},
		{"cel syntax", base(`{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"inputs.a =="}}`, ``), IssueCEL},
		{"reference in condition", base(`{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"${inputs.a} == 'x'"}}`, ``), IssueCEL},
		{"cel not an ancestor", base(`{"id":"x","type":"catalog.get","typeVersion":1,"parameters":{"id":"ORI-001"}},{"id":"y","type":"flow.if","typeVersion":1,"parameters":{"condition":"nodes.x.output.id == 'a'"}}`, ``), IssueNotAncestor},
		{"cel undeclared input", base(`{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"inputs.nope == 'x'"}}`, ``), IssueUnknownInput},
		{"cel event", base(`{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"event.type == 'x'"}}`, ``), IssueEventReference},
		{"cel dynamic node", base(`{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"nodes[inputs.a].output.y"}}`, ``), IssueCEL},
		{"literal secret", base(`{"id":"x","type":"mediacms.register","typeVersion":1,"parameters":{"assetId":"ORI-001","apiToken":"abc"}}`, ``), IssueSecret},
		{"approvers loosen", base(`{"id":"x","type":"approval.request","typeVersion":1,"parameters":{"kind":"publication","assetId":"ORI-001","approvers":["editor"]}}`, ``), IssueApprovers},
		{"approval other", base(`{"id":"x","type":"approval.request","typeVersion":1,"parameters":{"kind":"other","assetId":"ORI-001"}}`, ``), IssueApprovers},
		{"approval kind reference", base(`{"id":"x","type":"approval.request","typeVersion":1,"parameters":{"kind":"${inputs.a}","assetId":"ORI-001"}}`, ``), IssueApprovers},
		{"submitted bundle", `{"id":"t","version":"1.0.0","nodes":[{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}],"subworkflows":{}}`, IssueSnapshotField},
		{"cron", `{"id":"t","version":"1.0.0","trigger":{"type":"schedule","every":"1h","cron":"* * * * *"},"nodes":[{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`, IssueTrigger},
		{"bad every", `{"id":"t","version":"1.0.0","trigger":{"type":"schedule","every":"soon"},"nodes":[{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`, IssueTrigger},
		{"short every", `{"id":"t","version":"1.0.0","trigger":{"type":"schedule","every":"1s"},"nodes":[{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`, IssueTrigger},
		{"bad filter", `{"id":"t","version":"1.0.0","trigger":{"type":"event","event":"x","filter":"inputs.a"},"nodes":[{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`, IssueCEL},
		{"trigger with required input", `{"id":"t","version":"1.0.0","inputs":{"a":{"type":"string","required":true}},"trigger":{"type":"event","event":"x"},"nodes":[{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`, IssueTrigger},
		{"input default type", `{"id":"t","version":"1.0.0","inputs":{"a":{"type":"number","default":"x"}},"nodes":[{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`, IssueInput},
		{"bad output", `{"id":"t","version":"1.0.0","outputs":{"o":"${nodes.z.output.id}"},"nodes":[{"id":"x","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`, IssueReference},
		{"output field", base(`{"id":"x","type":"catalog.register","typeVersion":1,"parameters":{"kind":"original","title":"t"}},{"id":"y","type":"catalog.get","typeVersion":1,"parameters":{"id":"${nodes.x.output.nope}"}}`, `"x":["y"]`), IssueOutputField},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := v.Validate([]byte(c.doc))
			if !hasIssue(res, c.code) {
				t.Fatalf("expected issue %s, got %+v", c.code, res.Issues)
			}
		})
	}
}

func TestSpecExamples(t *testing.T) {
	v := NewValidator(nil)
	// §6.4 as printed: its parameters are valid, references hold, and the gate is there.
	data, err := os.ReadFile(filepath.Join("..", "..", "schemas", "testdata", "workflow-spec-6.4.json"))
	if err != nil {
		t.Fatal(err)
	}
	res := v.Validate(data)
	for _, i := range res.Issues {
		if i.Severity == SeverityError {
			t.Errorf("§6.4: %+v", i)
		}
	}
	// §8.2 runs an event trigger with a CEL filter.
	data, err = os.ReadFile(filepath.Join("..", "..", "schemas", "testdata", "workflow-spec-8.2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res := v.Validate(data); !res.OK() {
		t.Errorf("§8.2: %+v", res.Issues)
	}
	// §6.3 is a partial example: ai.index misses assetId, as the spec's R26 notes it
	// is minimal. Everything else about it must hold.
	data, err = os.ReadFile(filepath.Join("..", "..", "schemas", "testdata", "workflow-spec-6.3.json"))
	if err != nil {
		t.Fatal(err)
	}
	res = v.Validate(data)
	for _, i := range res.Issues {
		if i.Severity == SeverityError && !(i.Code == IssueParameters && i.NodeID == "index") {
			t.Errorf("§6.3: %+v", i)
		}
	}
}

func clipDoc(gateKind, cutSegments string, connections string) string {
	return `{"id":"my-clip","version":"1.0.0","nodes":[
		{"id":"source","type":"catalog.get","typeVersion":1,"parameters":{"id":"ORI-001"}},
		{"id":"gate","type":"approval.request","typeVersion":1,"parameters":{"kind":"` + gateKind + `","assetId":"ORI-001"}},
		{"id":"render","type":"video.cut","typeVersion":1,"parameters":{"source":"${nodes.source.output.working_master_uri}","segments":` + cutSegments + `}}
	],"connections":{` + connections + `}}`
}

func TestClipSelectionGate(t *testing.T) {
	v := NewValidator(nil)
	ok := `"source":["gate"],"gate":["render"]`
	mustValid(t, v, clipDoc("clip_selection", `"${nodes.gate.output.segments}"`, ok))

	for name, doc := range map[string]string{
		"no gate at all": `{"id":"my-clip","version":"1.0.0","nodes":[
			{"id":"render","type":"video.cut","typeVersion":1,"parameters":{"source":"storage://master/a.mp4","segments":[{"start_seconds":1,"end_seconds":2}]}}]}`,
		"wrong kind":        clipDoc("publication", `"${nodes.gate.output.segments}"`, ok),
		"literal segments":  clipDoc("clip_selection", `[{"start_seconds":1,"end_seconds":2}]`, ok),
		"bypassing path":    clipDoc("clip_selection", `"${nodes.gate.output.segments}"`, `"source":["gate","render"],"gate":["render"]`),
		"gate after render": clipDoc("clip_selection", `"${nodes.gate.output.segments}"`, `"source":["render"],"render":["gate"]`),
		"continue on failure": `{"id":"my-clip","version":"1.0.0","nodes":[
			{"id":"gate","type":"approval.request","typeVersion":1,"continueOnFailure":true,"parameters":{"kind":"clip_selection","assetId":"ORI-001"}},
			{"id":"render","type":"video.cut","typeVersion":1,"parameters":{"source":"storage://master/a.mp4","segments":"${nodes.gate.output.segments}"}}
		],"connections":{"gate":["render"]}}`,
		"merge bypass": `{"id":"my-clip","version":"1.0.0","nodes":[
			{"id":"source","type":"catalog.get","typeVersion":1,"parameters":{"id":"ORI-001"}},
			{"id":"gate","type":"approval.request","typeVersion":1,"parameters":{"kind":"clip_selection","assetId":"ORI-001"}},
			{"id":"join","type":"flow.merge","typeVersion":1,"parameters":{"mode":"any"}},
			{"id":"render","type":"video.cut","typeVersion":1,"parameters":{"source":"storage://master/a.mp4","segments":"${nodes.gate.output.segments}"}}
		],"connections":{"source":["gate","join"],"gate":["join"],"join":["render"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			res := v.Validate([]byte(doc))
			if !hasIssue(res, IssueClipSelectionGate) {
				t.Fatalf("expected the gate rule to refuse this, got %+v", res.Issues)
			}
		})
	}
	// Through the root-timeline mapping of D36.
	mustValid(t, v, `{"id":"my-clip","version":"1.0.0","nodes":[
		{"id":"gate","type":"approval.request","typeVersion":1,"parameters":{"kind":"clip_selection","assetId":"ORI-001"}},
		{"id":"map","type":"catalog.resolve_render_source","typeVersion":1,"parameters":{"sourceId":"ORI-001","segments":"${nodes.gate.output.segments}"}},
		{"id":"render","type":"video.cut","typeVersion":1,"parameters":{"source":"${nodes.map.output.source_uri}","segments":"${nodes.map.output.segments}"}}
	],"connections":{"gate":["map"],"map":["render"]}}`)
}

func registryWith(t *testing.T, docs ...string) (*Registry, *Validator) {
	t.Helper()
	v := NewValidator(nil)
	r := NewRegistry(v)
	for i, d := range docs {
		if err := r.Add([]byte(d), "doc"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	return r, v
}

const childDoc = `{"id":"child","version":"1.0.0","inputs":{"x":{"type":"string","required":true},"y":{"type":"string"}},
	"nodes":[{"id":"n","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}],"outputs":{"out":"${inputs.x}"}}`

func TestSubworkflowChecks(t *testing.T) {
	r, v := registryWith(t, childDoc)
	parent := func(params string) string {
		return `{"id":"parent","version":"1.0.0","inputs":{"a":{"type":"string"}},"nodes":[
			{"id":"call","type":"workflow.execute","typeVersion":1,"parameters":` + params + `},
			{"id":"after","type":"catalog.get","typeVersion":1,"parameters":{"id":"${nodes.call.output.out}"}}
		],"connections":{"call":["after"]}}`
	}
	mustValid(t, v, parent(`{"workflow":"child","x":"${inputs.a}"}`))
	for name, c := range map[string]struct {
		doc, code string
	}{
		"missing":    {parent(`{"workflow":"nope","x":"1"}`), IssueSubworkflow},
		"dynamic":    {parent(`{"workflow":"${inputs.a}","x":"1"}`), IssueSubworkflow},
		"required":   {parent(`{"workflow":"child"}`), IssueSubworkflow},
		"undeclared": {parent(`{"workflow":"child","x":"1","z":"2"}`), IssueSubworkflow},
	} {
		t.Run(name, func(t *testing.T) {
			if res := v.Validate([]byte(c.doc)); !hasIssue(res, c.code) {
				t.Fatalf("expected %s, got %+v", c.code, res.Issues)
			}
		})
	}
	res := v.Validate([]byte(`{"id":"parent","version":"1.0.0","nodes":[
		{"id":"call","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"child","x":"1"}},
		{"id":"after","type":"catalog.get","typeVersion":1,"parameters":{"id":"${nodes.call.output.nope}"}}
	],"connections":{"call":["after"]}}`))
	if !hasIssue(res, IssueOutputField) {
		t.Fatalf("expected a warning about the unknown output, got %+v", res.Issues)
	}
	foreach := func(params string) string {
		return `{"id":"parent","version":"1.0.0","nodes":[{"id":"each","type":"flow.foreach","typeVersion":1,"parameters":` + params + `}]}`
	}
	mustValid(t, v, foreach(`{"items":["a"],"workflow":"child","itemInput":"x"}`))
	if res := v.Validate([]byte(foreach(`{"items":["a"],"workflow":"child"}`))); !hasIssue(res, IssueSubworkflow) {
		t.Fatalf("default itemInput names an undeclared input: %+v", res.Issues)
	}
	// Recursion.
	if err := r.Add([]byte(`{"id":"loop-a","version":"1.0.0","nodes":[{"id":"n","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"loop-b"}}]}`), "a"); err != nil {
		t.Fatal(err)
	}
	if err := r.Add([]byte(`{"id":"loop-b","version":"1.0.0","nodes":[{"id":"n","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"loop-a"}}]}`), "b"); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("recursion must be refused: %v", err)
	}
}

func TestSnapshotBundle(t *testing.T) {
	r, _ := registryWith(t,
		`{"id":"leaf","version":"1.0.0","nodes":[{"id":"n","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`,
		`{"id":"mid","version":"2.0.0","nodes":[{"id":"n","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"leaf","version":"1.0.0"}}]}`,
	)
	root := []byte(`{"id":"root","version":"1.0.0","nodes":[{"id":"n","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"mid"}}]}`)
	snap, err := BuildSnapshot(root, RawFromRegistry(r))
	if err != nil {
		t.Fatal(err)
	}
	def, err := wfdef.Parse(snap)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := def.Subworkflows["mid"]; !ok {
		t.Fatalf("bundle %v", def.Subworkflows)
	}
	if _, ok := def.Subworkflows["leaf@1.0.0"]; !ok {
		t.Fatalf("bundle %v", def.Subworkflows)
	}
	// A child's snapshot comes from the parent's bundle, even after the registry moved.
	if err := r.Add([]byte(`{"id":"leaf","version":"1.0.0","nodes":[{"id":"changed","type":"flow.if","typeVersion":1,"parameters":{"condition":"false"}}]}`), "new"); err != nil {
		t.Fatal(err)
	}
	child, ok := RawFromBundle(def.Subworkflows)("mid", "")
	if !ok {
		t.Fatal("no child")
	}
	childSnap, err := BuildSnapshot(child, RawFromBundle(def.Subworkflows))
	if err != nil {
		t.Fatal(err)
	}
	childDef, _ := wfdef.Parse(childSnap)
	leaf, ok := childDef.Subworkflows["leaf@1.0.0"]
	if !ok || !strings.Contains(string(leaf), `"id":"n"`) {
		t.Fatalf("the snapshot must keep the old leaf: %s", leaf)
	}
	// No subworkflows: the snapshot is exactly the submitted bytes.
	leafOnly := []byte(`{"id":"plain",   "version":"1.0.0","nodes":[{"id":"n","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`)
	same, err := BuildSnapshot(leafOnly, RawFromRegistry(r))
	if err != nil || string(same) != string(leafOnly) {
		t.Fatalf("snapshot %q %v", same, err)
	}
	if _, err := BuildSnapshot([]byte(`{"id":"x","version":"1.0.0","nodes":[{"id":"n","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"absent"}}]}`), RawFromRegistry(r)); !errs.IsCode(err, errs.CodeValidationFailed) {
		t.Fatalf("missing child: %v", err)
	}
}

func TestRequiredPermissions(t *testing.T) {
	publishOne := `{"id":"pub-one","version":"1.0.0","inputs":{"assetId":{"type":"string"}},"nodes":[
		{"id":"prep","type":"publish.prepare","typeVersion":1,"parameters":{"assetId":"${inputs.assetId}","destination":"d"}}]}`
	gated := `{"id":"pub-gated","version":"1.0.0","nodes":[
		{"id":"gate","type":"approval.request","typeVersion":1,"parameters":{"kind":"publication","assetId":"ORI-001"}},
		{"id":"go","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"pub-one","assetId":"ORI-001"}}
	],"connections":{"gate":["go"]}}`
	r, _ := registryWith(t, publishOne, gated)
	perms := func(id string) string {
		def, _ := r.LookupWorkflow(id, "")
		var out []string
		for _, p := range RequiredPermissions(def, r) {
			out = append(out, string(p))
		}
		return strings.Join(out, ",")
	}
	if got := perms("pub-one"); got != "publication.create" {
		t.Fatalf("ungated: %q", got)
	}
	if got := perms("pub-gated"); got != "" {
		t.Fatalf("a publication gate authorizes its dominated children: %q", got)
	}
	def, err := wfdef.Parse([]byte(`{"id":"d","version":"1.0.0","nodes":[
		{"id":"a","type":"catalog.delete","typeVersion":1,"parameters":{"assetId":"ORI-001"}},
		{"id":"b","type":"storage.get","typeVersion":1,"parameters":{"source":"storage://archive/x","allowArchive":true}},
		{"id":"c","type":"publish.withdraw","typeVersion":1,"parameters":{"assetId":"ORI-001","all":true}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := RequiredPermissions(def, nil)
	if len(got) != 3 || got[0] != domain.PermAssetDelete || got[1] != domain.PermPublicationWithdraw || got[2] != domain.PermStorageArchiveRestore {
		t.Fatalf("required %v", got)
	}
}

func TestBindInputs(t *testing.T) {
	def, err := wfdef.Parse([]byte(`{"id":"t","version":"1.0.0","nodes":[{"id":"n","type":"flow.if","typeVersion":1}],"inputs":{
		"file":{"type":"file","required":true},
		"title":{"type":"string","required":true},
		"category":{"type":"string","default":"other"},
		"n":{"type":"number"},"flag":{"type":"boolean"},"list":{"type":"array"},"obj":{"type":"object"},"opt":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	file := func(_, v string) (string, error) {
		u, err := domain.ParseUserInputURI(v)
		if err != nil {
			return "", errs.Wrap(errs.CodeInvalidArgument, err, err.Error())
		}
		return u.String(), nil
	}
	out, err := BindInputs(def, json.RawMessage(`{"file":"/data/a.mp4","title":"T","n":3,"flag":true,"list":[1],"obj":{"a":1}}`), BindOptions{File: file})
	if err != nil {
		t.Fatal(err)
	}
	if out["file"] != "server:/data/a.mp4" || out["category"] != "other" {
		t.Fatalf("bound %v", out)
	}
	if _, ok := out["opt"]; ok {
		t.Fatal("an unset optional input stays absent")
	}
	for _, raw := range []string{
		`{"title":"T"}`,
		`{"file":"scratch://01J0000000000000000000000/n/f","title":"T"}`,
		`{"file":"/a","title":"T","nope":1}`,
		`{"file":"/a","title":1}`,
		`{"file":"/a","title":"T","n":"three"}`,
		`[1]`,
	} {
		if _, err := BindInputs(def, json.RawMessage(raw), BindOptions{File: file}); !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%s: %v", raw, err)
		}
	}
	if _, err := BindInputs(def, nil, BindOptions{AllowMissing: true}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
}

func TestDryRunR37AndChecks(t *testing.T) {
	v := NewValidator(nil)
	tenant := &config.Tenant{
		Storages: config.Storages{Roles: map[domain.StorageRole]config.StorageInstance{
			domain.RoleMaster:  {Driver: "local", Root: "/m", Usage: config.UsageOperational},
			domain.RoleProxy:   {Driver: "local", Root: "/p", Usage: config.UsageBackupOnly},
			domain.RoleArchive: {Driver: "gcs", Bucket: "a", CredentialsRef: "gcs-main"},
		}},
		Publishing: config.Publishing{Destinations: map[string]config.Destination{
			"yt": {Driver: "youtube", CredentialsRef: "youtube-oauth"},
		}},
	}
	infos := []connector.Info{
		{Family: connector.FamilyStorage, Driver: "local", Available: true, Capabilities: map[string]bool{"checksum_verification": true, "range_reads": false, "direct_access": true}},
		{Family: connector.FamilyPublishing, Driver: "youtube", Available: true},
	}
	secrets := []domain.SecretMeta{{Name: "youtube-oauth", Location: domain.SecretClient}}
	eventDoc := `{"id":"ev","version":"1.0.0","trigger":{"type":"event","event":"publication.completed"},"nodes":[
		{"id":"up","type":"publish.upload","typeVersion":1,"parameters":{"publicationId":"PUB-001"}},
		{"id":"w","type":"publish.withdraw","typeVersion":1,"parameters":{"assetId":"ORI-001","destinations":["yt"]}}]}`
	res := mustValid(t, v, eventDoc)
	issues := v.DryRun(res, DryRunEnv{Tenant: tenant, Connectors: infos, Secrets: secrets, SecretsKnown: true, SkipPermissions: true})
	found := false
	for _, i := range issues {
		if i.Code == IssueCredential && i.Severity == SeverityWarning {
			found = true
		}
	}
	if !found {
		t.Fatalf("R37 warning missing: %+v", issues)
	}
	// The same credential on a CLI workflow is fine: a client may be connected.
	cli := mustValid(t, v, strings.Replace(eventDoc, `"trigger":{"type":"event","event":"publication.completed"},`, "", 1))
	for _, i := range v.DryRun(cli, DryRunEnv{Tenant: tenant, Connectors: infos, Secrets: secrets, SecretsKnown: true, SkipPermissions: true}) {
		if i.Code == IssueCredential {
			t.Fatalf("unexpected: %+v", i)
		}
	}
	// Permissions, unconfigured destination, role and capabilities.
	doc := `{"id":"x","version":"1.0.0","nodes":[
		{"id":"a","type":"catalog.delete","typeVersion":1,"parameters":{"assetId":"ORI-001"}},
		{"id":"b","type":"publish.withdraw","typeVersion":1,"parameters":{"assetId":"ORI-001","destinations":["nope"]}},
		{"id":"c","type":"storage.put","typeVersion":1,"parameters":{"storage":"archive","source":"server:/a","key":"k"}},
		{"id":"d","type":"approval.request","typeVersion":1,"parameters":{"kind":"clip_selection","assetId":"ORI-001"}},
		{"id":"e","type":"video.cut","typeVersion":1,"parameters":{"source":"storage://master/k","segments":"${nodes.d.output.segments}"}},
		{"id":"f","type":"video.preview","typeVersion":1,"parameters":{"source":"storage://proxy/k","segments":[{"start_seconds":1,"end_seconds":2}]}}
	],"connections":{"d":["e"]}}`
	res = mustValid(t, v, doc)
	got := map[string]int{}
	for _, i := range v.DryRun(res, DryRunEnv{Principal: domain.Principal{Role: domain.RoleEditor}, Tenant: tenant, Connectors: infos, WorkersKnown: true}) {
		got[i.Code]++
	}
	if got[IssuePermission] == 0 || got[IssueConnector] < 2 || got[IssueCapability] < 2 || got[IssueResources] == 0 {
		t.Fatalf("dry run findings: %+v", got)
	}
	// Plugins and resources.
	hooked := `{"id":"h","version":"1.0.0","hooks":{"before":[{"id":"v","plugin":"asset-validator","action":"check"}]},"nodes":[{"id":"n","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`
	res = mustValid(t, v, hooked)
	issues = v.DryRun(res, DryRunEnv{SkipPermissions: true, PluginsKnown: true})
	if len(issues) != 1 || issues[0].Code != IssuePlugin {
		t.Fatalf("plugin: %+v", issues)
	}
}

func TestLoadRegistry(t *testing.T) {
	shipped := fstest.MapFS{"a.json": {Data: []byte(`{"id":"a","version":"1.0.0","nodes":[{"id":"n","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`)}}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"id":"a","version":"1.1.0","nodes":[{"id":"n","type":"flow.if","typeVersion":1,"parameters":{"condition":"false"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRegistry(NewValidator(nil), shipped, []string{dir, filepath.Join(dir, "absent")})
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := r.Get("a", ""); !ok || e.Definition.Version != "1.1.0" {
		t.Fatalf("latest %+v", e)
	}
	bad := fstest.MapFS{"b.json": {Data: []byte(`{"id":"b","version":"1.0.0","nodes":[{"id":"n","type":"nope.nope","typeVersion":1}]}`)}}
	if _, err := LoadRegistry(NewValidator(nil), bad, nil); err == nil {
		t.Fatal("an invalid shipped file must stop the daemon")
	}
}

func TestRegistry(t *testing.T) {
	dir := t.TempDir()
	v := NewValidator(nil)
	r := NewRegistry(v)
	write := func(name, doc string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	doc := func(id, version string) string {
		return `{"id":"` + id + `","version":"` + version + `","nodes":[{"id":"n","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`
	}
	write("a.json", doc("a", "1.2.0"))
	write("a2.json", doc("a", "1.10.0"))
	write("note.txt", "ignored")
	if err := r.LoadDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadDir(filepath.Join(dir, "missing")); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(); err != nil {
		t.Fatal(err)
	}
	e, ok := r.Get("a", "")
	if !ok || e.Definition.Version != "1.10.0" {
		t.Fatalf("latest: %+v", e)
	}
	if _, ok := r.Get("a", "1.2.0"); !ok {
		t.Fatal("pinned version")
	}
	if len(r.List()) != 1 {
		t.Fatalf("list: %d", len(r.List()))
	}
	write("bad.json", `{"id":"bad","version":"1.0.0","nodes":[{"id":"n","type":"nope.x","typeVersion":1}]}`)
	if err := r.LoadDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(); err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("check: %v", err)
	}
}
