package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/schemas"
)

// Severities of a ValidationIssue.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Issue codes. Clients may rely on them.
const (
	IssueSchema            = "schema"
	IssueGraph             = "graph"
	IssueNodeType          = "unknown_node_type"
	IssueNodeUnavailable   = "node_unavailable"
	IssueParameters        = "parameters"
	IssueReference         = "reference"
	IssueNotAncestor       = "reference_not_ancestor"
	IssueUnknownInput      = "unknown_input"
	IssueEventReference    = "event_reference_without_trigger"
	IssueOutputField       = "unknown_output_field"
	IssueCEL               = "cel"
	IssueTrigger           = "trigger"
	IssueApprovers         = "approvers"
	IssueClipSelectionGate = "clip_selection_gate"
	IssuePublicationGate   = "publication_gate"
	IssueSubworkflow       = "subworkflow"
	IssueSecret            = "secret_in_definition"
	IssueInput             = "input"
	IssueSnapshotField     = "snapshot_field"
	IssuePool              = "pool"

	IssuePermission = "permission_denied"
	IssueConnector  = "connector"
	IssueCapability = "capability_unsupported"
	IssuePlugin     = "plugin"
	IssueResources  = "resources"
	IssueCredential = "client_credential"
	IssueUnknownRef = "unknown_credential"
)

// conditionParams are the top-level parameters that are CEL, not templates.
var conditionParams = map[string]map[string]bool{
	wfdef.TypeFlowIf: {"condition": true},
}

// SkipKeys returns the top-level parameter keys of a node type that hold CEL.
func SkipKeys(nodeType string) map[string]bool { return conditionParams[nodeType] }

// SubworkflowLookup finds a workflow's definition. An empty version means the
// latest. The registry implements it.
type SubworkflowLookup interface {
	LookupWorkflow(id, version string) (*wfdef.Definition, bool)
}

// Validator checks workflows. It is safe for concurrent use.
type Validator struct {
	reg  node.Registry
	subs SubworkflowLookup
}

// NewValidator returns a validator. reg lists the node types this process can
// run; nodes of pool engine and daemon must be registered there. A nil reg checks
// only against the catalog.
func NewValidator(reg node.Registry) *Validator { return &Validator{reg: reg} }

// UseSubworkflows sets where workflow.execute and flow.foreach targets are found.
// With none, targets are not checked.
func (v *Validator) UseSubworkflows(l SubworkflowLookup) { v.subs = l }

// Result is the outcome of validation.
type Result struct {
	Definition *wfdef.Definition
	Graph      *Graph
	Issues     []api.ValidationIssue
}

// OK reports whether no issue is an error.
func (r Result) OK() bool {
	for _, i := range r.Issues {
		if i.Severity == SeverityError {
			return false
		}
	}
	return true
}

// API returns the wire form.
func (r Result) API() api.ValidationResult {
	return api.ValidationResult{Valid: r.OK(), Issues: r.Issues}
}

// Err returns validation_failed naming the first errors, or nil.
func (r Result) Err() error {
	if r.OK() {
		return nil
	}
	var msgs []string
	for _, i := range r.Issues {
		if i.Severity == SeverityError {
			msgs = append(msgs, i.Message)
		}
		if len(msgs) == 5 {
			break
		}
	}
	return errs.Newf(errs.CodeValidationFailed, "invalid workflow: %s", strings.Join(msgs, "; ")).With("issues", r.Issues)
}

type issues struct{ list []api.ValidationIssue }

func (is *issues) add(sev, code, nodeID, path, format string, args ...any) {
	is.list = append(is.list, api.ValidationIssue{
		Severity: sev, Code: code, NodeID: nodeID, Path: path, Message: fmt.Sprintf(format, args...),
	})
}
func (is *issues) err(code, nodeID, path, format string, args ...any) {
	is.add(SeverityError, code, nodeID, path, format, args...)
}
func (is *issues) warn(code, nodeID, path, format string, args ...any) {
	is.add(SeverityWarning, code, nodeID, path, format, args...)
}

// Validate runs every static rule of plan section 4 on a workflow file.
func (v *Validator) Validate(data []byte) Result {
	res := v.validate(data)
	// A snapshot field in submitted text means someone tries to supply their own
	// subworkflows, which would skip the checks below (I144).
	if res.Definition != nil && res.Definition.Subworkflows != nil {
		res.Issues = append(res.Issues, api.ValidationIssue{
			Severity: SeverityError, Code: IssueSnapshotField, Path: "subworkflows",
			Message: "subworkflows is written by the engine in job snapshots and cannot be submitted",
		})
	}
	return res
}

func (v *Validator) validate(data []byte) Result {
	var is issues
	if err := schemas.Validate(schemas.Workflow, data); err != nil {
		reportSchema(&is, "", err)
		return Result{Issues: is.list}
	}
	def, err := wfdef.Parse(data)
	if err != nil {
		is.err(IssueSchema, "", "", "%v", err)
		return Result{Issues: is.list}
	}
	g, graphErrs := BuildGraph(def)
	for _, e := range graphErrs {
		is.err(IssueGraph, "", "connections", "%v", e)
	}
	res := Result{Definition: def, Graph: g}
	v.checkInputs(&is, def)
	v.checkTrigger(&is, def)
	for i := range def.Nodes {
		v.checkNode(&is, def, g, &def.Nodes[i], len(graphErrs) == 0)
	}
	checkOutputs(&is, def)
	if len(graphErrs) == 0 {
		checkClipGate(&is, def, g)
		checkPublicationGates(&is, def)
		v.checkRecursion(&is, def)
	}
	res.Issues = is.list
	return res
}

func reportSchema(is *issues, nodeID string, err error) {
	var ve *jsonschema.ValidationError
	if errors.As(err, &ve) {
		for _, u := range leaves(ve) {
			is.err(IssueSchema, nodeID, u.path, "%s: %s", locationOf(u.path), u.message)
		}
		return
	}
	is.err(IssueSchema, nodeID, "", "%v", err)
}

func locationOf(path string) string {
	if path == "" {
		return "document"
	}
	return path
}

type leaf struct {
	path    string
	message string
}

// leaves flattens a validation error to what actually failed.
func leaves(ve *jsonschema.ValidationError) []leaf {
	out := ve.BasicOutput()
	var res []leaf
	seen := map[string]bool{}
	for _, u := range out.Errors {
		if u.Error == nil {
			continue
		}
		key := u.InstanceLocation + "\x00" + u.Error.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		res = append(res, leaf{path: u.InstanceLocation, message: u.Error.String()})
	}
	return res
}

func (v *Validator) checkInputs(is *issues, def *wfdef.Definition) {
	for _, name := range sortedSpecKeys(def.Inputs) {
		spec := def.Inputs[name]
		if len(spec.Default) > 0 {
			val, err := DecodeJSON(spec.Default)
			if err != nil {
				is.err(IssueInput, "", "inputs."+name, "default of input %q is not valid JSON", name)
				continue
			}
			if err := checkInputType(spec.Type, val); err != nil {
				is.err(IssueInput, "", "inputs."+name, "default of input %q: %v", name, err)
			}
			findSecrets(is, "", "inputs."+name, name, val)
		}
	}
}

func sortedSpecKeys(m map[string]wfdef.InputSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkInputType checks a decoded value against a declared input type.
func checkInputType(t wfdef.InputType, v any) error {
	ok := false
	switch t {
	case wfdef.InputString, wfdef.InputFile:
		_, ok = v.(string)
	case wfdef.InputNumber:
		_, ok = v.(json.Number)
	case wfdef.InputBool:
		_, ok = v.(bool)
	case wfdef.InputArray:
		_, ok = v.([]any)
	case wfdef.InputObject:
		_, ok = v.(map[string]any)
	}
	if !ok {
		return fmt.Errorf("expected type %s", t)
	}
	return nil
}

// MinScheduleInterval is the shortest `every` (I155).
const MinScheduleInterval = time.Minute

func (v *Validator) checkTrigger(is *issues, def *wfdef.Definition) {
	t := def.Trigger
	if t == nil {
		return
	}
	if t.Cron != "" {
		is.err(IssueTrigger, "", "trigger.cron", "cron is reserved and not implemented; use every (I22)")
	}
	switch t.Type {
	case wfdef.TriggerWebhook:
		is.warn(IssueTrigger, "", "trigger.type", "webhook triggers validate but are refused when run (I22)")
	case wfdef.TriggerSchedule:
		d, err := time.ParseDuration(t.Every)
		if err != nil {
			is.err(IssueTrigger, "", "trigger.every", "every: %v", err)
		} else if d < MinScheduleInterval {
			is.err(IssueTrigger, "", "trigger.every", "every must be at least %s", MinScheduleInterval)
		}
	case wfdef.TriggerEvent:
		if t.Filter != "" {
			if _, err := CompileFilter(t.Filter); err != nil {
				is.err(IssueCEL, "", "trigger.filter", "filter: %v", err)
			}
		}
	}
	if t.Type == wfdef.TriggerEvent || t.Type == wfdef.TriggerSchedule {
		for _, name := range sortedSpecKeys(def.Inputs) {
			if spec := def.Inputs[name]; spec.Required && len(spec.Default) == 0 {
				is.err(IssueTrigger, "", "inputs."+name, "input %q is required, but nothing supplies inputs when trigger %s starts this workflow", name, t.Type)
			}
		}
	}
}

// Descriptor returns a node type's contract: the registered node's when there is
// one, otherwise the catalog's. Worker-pool nodes are not registered in the daemon.
func (v *Validator) Descriptor(nodeType string, version int) (node.Descriptor, bool) {
	if v.reg != nil {
		if n, ok := v.reg.Lookup(nodeType, version); ok {
			return n.Descriptor(), true
		}
	}
	d, err := schemas.DescriptorFor(nodeType, version)
	return d, err == nil
}

func parameterSchema(d node.Descriptor) (*jsonschema.Schema, error) {
	cat, err := schemas.NodeCatalog()
	if err != nil {
		return nil, err
	}
	if e, ok := cat.Lookup(d.Type, d.TypeVersion); ok {
		return schemas.Compile(e.ParametersRef())
	}
	return compileRaw(d.Parameters)
}

func compileRaw(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("node-parameters.json", doc); err != nil {
		return nil, err
	}
	return c.Compile("node-parameters.json")
}

// ValidateResolved applies the full parameter schema to resolved parameters, which
// is what runs before a node starts (catalog description, §6.2).
func ValidateResolved(d node.Descriptor, params json.RawMessage) error {
	sch, err := parameterSchema(d)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot compile node parameter schema")
	}
	if sch == nil {
		return nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(params))
	if err != nil {
		return errs.Wrap(errs.CodeInvalidArgument, err, "node parameters are not valid JSON")
	}
	if err := sch.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			var msgs []string
			for _, l := range leaves(ve) {
				msgs = append(msgs, locationOf(l.path)+": "+l.message)
			}
			return errs.Newf(errs.CodeValidationFailed, "node %s parameters: %s", d.Type, strings.Join(msgs, "; "))
		}
		return errs.Wrap(errs.CodeValidationFailed, err, "invalid node parameters")
	}
	return nil
}

func (v *Validator) checkNode(is *issues, def *wfdef.Definition, g *Graph, n *wfdef.NodeSpec, graphOK bool) {
	d, ok := v.Descriptor(n.Type, n.TypeVersion)
	if !ok {
		is.err(IssueNodeType, n.ID, "type", "node %q: unknown node type %s@%d", n.ID, n.Type, n.TypeVersion)
		return
	}
	if v.reg != nil && (d.Pool == node.PoolEngine || d.Pool == node.PoolDaemon) {
		if _, registered := v.reg.Lookup(n.Type, n.TypeVersion); !registered {
			is.err(IssueNodeUnavailable, n.ID, "type", "node %q: %s@%d runs in the daemon but is not registered", n.ID, n.Type, n.TypeVersion)
		}
	}
	if d.SideEffect == node.EffectReconcilable && d.Pool != node.PoolDaemon {
		is.warn(IssuePool, n.ID, "type", "node %q: %s is reconcilable outside the daemon pool, so any failure goes to needs_attention (I148)", n.ID, n.Type)
	}
	params := n.Parameters
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	val, err := DecodeJSON(params)
	if err != nil {
		is.err(IssueParameters, n.ID, "parameters", "node %q: parameters are not valid JSON", n.ID)
		return
	}
	checkLiteralParameters(is, d, n, params, val)
	findSecrets(is, n.ID, "parameters", "", val)
	v.checkReferences(is, def, g, n, val, graphOK)
	switch n.Type {
	case wfdef.TypeFlowIf:
		v.checkCondition(is, def, g, n, val, graphOK)
	case wfdef.TypeApprovalRequest:
		checkApproval(is, n, val)
	case wfdef.TypeWorkflowExecute:
		v.checkExecute(is, n, val)
	case wfdef.TypeFlowForeach:
		v.checkForeach(is, n, val)
	}
}

// checkLiteralParameters applies the catalog schema, ignoring failures located at a
// value that holds a reference: only required keys and literal types can be known
// before the job runs (catalog description).
func checkLiteralParameters(is *issues, d node.Descriptor, n *wfdef.NodeSpec, raw json.RawMessage, val any) {
	sch, err := parameterSchema(d)
	if err != nil {
		is.err(IssueParameters, n.ID, "parameters", "node %q: cannot compile its parameter schema: %v", n.ID, err)
		return
	}
	if sch == nil {
		return
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		is.err(IssueParameters, n.ID, "parameters", "node %q: %v", n.ID, err)
		return
	}
	err = sch.Validate(doc)
	if err == nil {
		return
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		is.err(IssueParameters, n.ID, "parameters", "node %q: %v", n.ID, err)
		return
	}
	for _, l := range leaves(ve) {
		if holdsRef(val, l.path) {
			continue
		}
		is.err(IssueParameters, n.ID, "parameters"+l.path, "node %q (%s): %s: %s", n.ID, n.Type, locationOf(l.path), l.message)
	}
}

// holdsRef reports whether the value at a JSON pointer, or anything above it, is a
// string with a reference.
func holdsRef(v any, pointer string) bool {
	if s, ok := v.(string); ok && HasRef(s) {
		return true
	}
	if pointer == "" {
		return false
	}
	for _, seg := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		next, ok := walkPath(v, []string{seg})
		if !ok {
			return false
		}
		if s, ok := next.(string); ok && HasRef(s) {
			return true
		}
		v = next
	}
	return false
}

var secretKey = regexp.MustCompile(`(?i)(password|passwd|passphrase|secret|token|api[_-]?key|private[_-]?key|access[_-]?key|bearer|client[_-]?secret)$`)

// findSecrets refuses literal values under keys that name a credential: workflow
// files carry only credentialsRef (§9.1, §15.6).
func findSecrets(is *issues, nodeID, path, key string, v any) {
	switch x := v.(type) {
	case string:
		if key != "" && secretKey.MatchString(key) && x != "" && !HasRef(x) {
			is.err(IssueSecret, nodeID, path, "%s looks like a literal secret; workflow files carry only credentialsRef (§9.1)", path)
		}
	case []any:
		for _, e := range x {
			findSecrets(is, nodeID, path, key, e)
		}
	case map[string]any:
		for k, e := range x {
			findSecrets(is, nodeID, path+"."+k, k, e)
		}
	}
}

func (v *Validator) checkReferences(is *issues, def *wfdef.Definition, g *Graph, n *wfdef.NodeSpec, val any, graphOK bool) {
	err := CollectRefs(val, SkipKeys(n.Type), func(r Ref) {
		v.checkRef(is, def, g, n.ID, r, graphOK)
	})
	if err != nil {
		is.err(IssueReference, n.ID, "parameters", "node %q: %v", n.ID, err)
	}
}

func (v *Validator) checkRef(is *issues, def *wfdef.Definition, g *Graph, nodeID string, r Ref, graphOK bool) {
	switch r.Kind {
	case RefInputs:
		if _, ok := def.Inputs[r.Path[0]]; !ok {
			is.err(IssueUnknownInput, nodeID, "", "%s names an input the workflow does not declare", r.Raw)
		}
	case RefEvent:
		if def.Trigger == nil || def.Trigger.Type != wfdef.TriggerEvent {
			is.err(IssueEventReference, nodeID, "", "%s needs an event trigger", r.Raw)
		}
	case RefNodes:
		if _, ok := g.Index[r.Node]; !ok {
			is.err(IssueReference, nodeID, "", "%s names an unknown node", r.Raw)
			return
		}
		if nodeID != "" && graphOK && !g.IsAncestor(r.Node, nodeID) {
			is.err(IssueNotAncestor, nodeID, "", "%s requires %q to be an ancestor of %q in connections (R24)", r.Raw, r.Node, nodeID)
			return
		}
		v.checkOutputPath(is, def, nodeID, r)
	}
}

// checkOutputPath warns about a field the referenced node does not declare. The
// contract between workflows and nodes is the catalog, so a typo here would only
// surface as a missing parameter in a running job.
func (v *Validator) checkOutputPath(is *issues, def *wfdef.Definition, nodeID string, r Ref) {
	if len(r.Path) == 0 {
		return
	}
	spec := findNode(def, r.Node)
	if spec == nil {
		return
	}
	if spec.Type == wfdef.TypeWorkflowExecute {
		if v.subs == nil {
			return
		}
		name, version := executeTarget(spec)
		if name == "" {
			return
		}
		child, ok := v.subs.LookupWorkflow(name, version)
		if !ok {
			return
		}
		if _, ok := child.Outputs[r.Path[0]]; !ok {
			is.warn(IssueOutputField, nodeID, "", "%s: workflow %s has no output %q", r.Raw, name, r.Path[0])
		}
		return
	}
	d, ok := v.Descriptor(spec.Type, spec.TypeVersion)
	if !ok {
		return
	}
	if missing, known := outputField(d.Output, r.Path); known && missing != "" {
		is.warn(IssueOutputField, nodeID, "", "%s: %s does not declare output %q", r.Raw, spec.Type, missing)
	}
}

func findNode(def *wfdef.Definition, id string) *wfdef.NodeSpec {
	for i := range def.Nodes {
		if def.Nodes[i].ID == id {
			return &def.Nodes[i]
		}
	}
	return nil
}

func executeTarget(n *wfdef.NodeSpec) (name, version string) {
	var p struct {
		Workflow string `json:"workflow"`
		Version  string `json:"version"`
	}
	if json.Unmarshal(n.Parameters, &p) != nil || HasRef(p.Workflow) {
		return "", ""
	}
	return p.Workflow, p.Version
}

// outputField walks an output schema along a path. It returns the first segment
// the schema says cannot exist; known is false when the schema does not say.
func outputField(raw json.RawMessage, path []string) (missing string, known bool) {
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		return "", false
	}
	defs := catalogDefs()
	current := schema
	for _, seg := range path {
		current = deref(current, defs)
		if current == nil {
			return "", false
		}
		if t, _ := current["type"].(string); t == "array" {
			items, _ := current["items"].(map[string]any)
			current = items
			continue
		}
		props, ok := current["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			return "", false
		}
		if extra, ok := current["additionalProperties"].(bool); ok && extra {
			return "", false
		}
		next, ok := props[seg].(map[string]any)
		if !ok {
			return seg, true
		}
		current = next
	}
	return "", true
}

func deref(s map[string]any, defs map[string]any) map[string]any {
	ref, ok := s["$ref"].(string)
	if !ok {
		return s
	}
	name, found := strings.CutPrefix(ref, "#/$defs/")
	if !found {
		return nil
	}
	out, _ := defs[name].(map[string]any)
	return out
}

func catalogDefs() map[string]any {
	data, err := schemas.Read(schemas.NodeCatalogFile)
	if err != nil {
		return nil
	}
	var doc struct {
		Defs map[string]any `json:"$defs"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	return doc.Defs
}

func (v *Validator) checkCondition(is *issues, def *wfdef.Definition, g *Graph, n *wfdef.NodeSpec, val any, graphOK bool) {
	src, _ := walkPath(val, []string{"condition"})
	s, ok := src.(string)
	if !ok {
		return
	}
	if strings.Contains(s, "${") {
		is.err(IssueCEL, n.ID, "parameters.condition", "node %q: ${...} is only for data references; conditions are CEL (D2)", n.ID)
		return
	}
	c, err := CompileCondition(s)
	if err != nil {
		is.err(IssueCEL, n.ID, "parameters.condition", "node %q: %v", n.ID, err)
		return
	}
	if c.DynamicNodes {
		is.err(IssueCEL, n.ID, "parameters.condition", "node %q: nodes must be indexed by a literal, so ancestors can be checked (R24)", n.ID)
	}
	for _, target := range c.Nodes {
		if _, ok := g.Index[target]; !ok {
			is.err(IssueReference, n.ID, "parameters.condition", "node %q: condition names unknown node %q", n.ID, target)
		} else if graphOK && !g.IsAncestor(target, n.ID) {
			is.err(IssueNotAncestor, n.ID, "parameters.condition", "condition of %q reads node %q, which must be its ancestor (R24)", n.ID, target)
		}
	}
	for _, name := range c.Inputs {
		if _, ok := def.Inputs[name]; !ok {
			is.err(IssueUnknownInput, n.ID, "parameters.condition", "condition of %q reads input %q, which the workflow does not declare", n.ID, name)
		}
	}
	if c.UsesEvent && (def.Trigger == nil || def.Trigger.Type != wfdef.TriggerEvent) {
		is.err(IssueEventReference, n.ID, "parameters.condition", "condition of %q reads event, which needs an event trigger", n.ID)
	}
}

func checkApproval(is *issues, n *wfdef.NodeSpec, val any) {
	kindVal, _ := walkPath(val, []string{"kind"})
	kind, _ := kindVal.(string)
	if HasRef(kind) {
		is.err(IssueApprovers, n.ID, "parameters.kind", "node %q: the approval kind must be literal, so the D7 rule can be checked", n.ID)
		return
	}
	approversVal, ok := walkPath(val, []string{"approvers"})
	if !ok {
		approversVal = nil
	}
	var approvers []string
	if list, ok := approversVal.([]any); ok {
		for _, e := range list {
			if s, ok := e.(string); ok {
				if HasRef(s) {
					is.err(IssueApprovers, n.ID, "parameters.approvers", "node %q: approvers must be literal, so a list that loosens D7 can be refused", n.ID)
					return
				}
				approvers = append(approvers, s)
			}
		}
	} else if s, ok := approversVal.(string); ok && HasRef(s) {
		is.err(IssueApprovers, n.ID, "parameters.approvers", "node %q: approvers must be literal, so a list that loosens D7 can be refused", n.ID)
		return
	}
	if err := domain.ValidateApprovers(domain.ApprovalKind(kind), approvers); err != nil {
		is.err(IssueApprovers, n.ID, "parameters.approvers", "node %q: %v", n.ID, err)
	}
}

func (v *Validator) checkExecute(is *issues, n *wfdef.NodeSpec, val any) {
	params, _ := val.(map[string]any)
	name, _ := params["workflow"].(string)
	if name == "" {
		return
	}
	if HasRef(name) {
		is.err(IssueSubworkflow, n.ID, "parameters.workflow", "node %q: the workflow must be named literally, so its permissions and snapshot are known at submission (I144)", n.ID)
		return
	}
	version, _ := params["version"].(string)
	if v.subs == nil {
		return
	}
	child, ok := v.subs.LookupWorkflow(name, version)
	if !ok {
		is.err(IssueSubworkflow, n.ID, "parameters.workflow", "node %q: workflow %s is not available", n.ID, name)
		return
	}
	given := map[string]bool{}
	for k := range params {
		if k != "workflow" && k != "version" {
			given[k] = true
		}
	}
	checkChildInputs(is, n.ID, child, given)
}

func (v *Validator) checkForeach(is *issues, n *wfdef.NodeSpec, val any) {
	params, _ := val.(map[string]any)
	name, _ := params["workflow"].(string)
	if name == "" {
		return
	}
	if HasRef(name) {
		is.err(IssueSubworkflow, n.ID, "parameters.workflow", "node %q: the workflow must be named literally (I144)", n.ID)
		return
	}
	version, _ := params["version"].(string)
	if v.subs == nil {
		return
	}
	child, ok := v.subs.LookupWorkflow(name, version)
	if !ok {
		is.err(IssueSubworkflow, n.ID, "parameters.workflow", "node %q: workflow %s is not available", n.ID, name)
		return
	}
	given := map[string]bool{}
	item, _ := params["itemInput"].(string)
	if item == "" {
		item = "item"
	}
	given[item] = true
	if inputs, ok := params["inputs"].(map[string]any); ok {
		for k := range inputs {
			given[k] = true
		}
	}
	checkChildInputs(is, n.ID, child, given)
}

func checkChildInputs(is *issues, nodeID string, child *wfdef.Definition, given map[string]bool) {
	names := make([]string, 0, len(given))
	for k := range given {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if _, ok := child.Inputs[k]; !ok {
			is.err(IssueSubworkflow, nodeID, "parameters."+k, "node %q: workflow %s does not declare input %q (input isolation, §6.2)", nodeID, child.ID, k)
		}
	}
	for _, k := range sortedSpecKeys(child.Inputs) {
		spec := child.Inputs[k]
		if spec.Required && len(spec.Default) == 0 && !given[k] {
			is.err(IssueSubworkflow, nodeID, "parameters."+k, "node %q: workflow %s requires input %q", nodeID, child.ID, k)
		}
	}
}

func checkOutputs(is *issues, def *wfdef.Definition) {
	names := make([]string, 0, len(def.Outputs))
	for k := range def.Outputs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		parts, err := ParseTemplate(def.Outputs[name])
		if err != nil {
			is.err(IssueReference, "", "outputs."+name, "output %q: %v", name, err)
			continue
		}
		for _, p := range parts {
			if p.Ref == nil {
				continue
			}
			switch p.Ref.Kind {
			case RefInputs:
				if _, ok := def.Inputs[p.Ref.Path[0]]; !ok {
					is.err(IssueUnknownInput, "", "outputs."+name, "output %q: %s names an undeclared input", name, p.Ref.Raw)
				}
			case RefNodes:
				if findNode(def, p.Ref.Node) == nil {
					is.err(IssueReference, "", "outputs."+name, "output %q: %s names an unknown node", name, p.Ref.Raw)
				}
			case RefEvent:
				if def.Trigger == nil || def.Trigger.Type != wfdef.TriggerEvent {
					is.err(IssueEventReference, "", "outputs."+name, "output %q: %s needs an event trigger", name, p.Ref.Raw)
				}
			}
		}
	}
}

// gates lists the approval.request nodes of a literal kind.
func gates(def *wfdef.Definition, kind domain.ApprovalKind) []*wfdef.NodeSpec {
	var out []*wfdef.NodeSpec
	for i := range def.Nodes {
		n := &def.Nodes[i]
		if n.Type != wfdef.TypeApprovalRequest {
			continue
		}
		var p struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(n.Parameters, &p) == nil && domain.ApprovalKind(p.Kind) == kind {
			out = append(out, n)
		}
	}
	return out
}

// checkClipGate enforces D53 on every workflow that renders a clip (I142).
func checkClipGate(is *issues, def *wfdef.Definition, g *Graph) {
	selection := gates(def, domain.ApprovalClipSelection)
	for _, gate := range selection {
		if gate.ContinueOnFailure {
			is.err(IssueClipSelectionGate, gate.ID, "continueOnFailure", "node %q: a clip_selection gate cannot continue on failure (D53)", gate.ID)
		}
	}
	for i := range def.Nodes {
		cut := &def.Nodes[i]
		if cut.Type != "video.cut" {
			continue
		}
		var dominating *wfdef.NodeSpec
		for _, gate := range selection {
			if g.Dominates(gate.ID, cut.ID) {
				dominating = gate
				break
			}
		}
		if dominating == nil {
			is.err(IssueClipSelectionGate, cut.ID, "", "node %q: every video.cut must follow an approval.request of kind clip_selection on every path; no policy can skip it (D53)", cut.ID)
			continue
		}
		if !segmentsFromGate(def, cut, dominating.ID) {
			is.err(IssueClipSelectionGate, cut.ID, "parameters.segments", "node %q: segments must be the approved segments of %q, not suggested or literal ones (R27, D53)", cut.ID, dominating.ID)
		}
	}
}

func segmentsParam(n *wfdef.NodeSpec) string {
	var p struct {
		Segments json.RawMessage `json:"segments"`
	}
	if json.Unmarshal(n.Parameters, &p) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(p.Segments, &s) != nil {
		return ""
	}
	return s
}

func segmentsFromGate(def *wfdef.Definition, cut *wfdef.NodeSpec, gate string) bool {
	direct := fmt.Sprintf("${nodes.%s.output.segments}", gate)
	s := segmentsParam(cut)
	if s == direct {
		return true
	}
	parts, err := ParseTemplate(s)
	if err != nil || len(parts) != 1 || parts[0].Ref == nil || parts[0].Ref.Kind != RefNodes {
		return false
	}
	via := findNode(def, parts[0].Ref.Node)
	return via != nil && via.Type == "catalog.resolve_render_source" &&
		len(parts[0].Ref.Path) == 1 && parts[0].Ref.Path[0] == "segments" &&
		segmentsParam(via) == direct
}

// checkPublicationGates keeps a publication gate from being passed by failure.
func checkPublicationGates(is *issues, def *wfdef.Definition) {
	for _, gate := range gates(def, domain.ApprovalPublication) {
		if gate.ContinueOnFailure {
			is.err(IssuePublicationGate, gate.ID, "continueOnFailure", "node %q: a publication gate cannot continue on failure (D7)", gate.ID)
		}
	}
}

// checkRecursion refuses cycles among subworkflows (I144).
func (v *Validator) checkRecursion(is *issues, def *wfdef.Definition) {
	if v.subs == nil {
		return
	}
	var visit func(d *wfdef.Definition, path []string) bool
	visit = func(d *wfdef.Definition, path []string) bool {
		for _, target := range ChildWorkflows(d) {
			for _, seen := range path {
				if seen == target.Workflow {
					is.err(IssueSubworkflow, "", "", "workflows call each other in a cycle: %s", strings.Join(append(path, target.Workflow), " -> "))
					return false
				}
			}
			child, ok := v.subs.LookupWorkflow(target.Workflow, target.Version)
			if !ok {
				continue // reported on the node when it is in the submitted file
			}
			if !visit(child, append(path, target.Workflow)) {
				return false
			}
		}
		return true
	}
	visit(def, []string{def.ID})
}

// ChildTarget is one subworkflow a definition names.
type ChildTarget struct {
	Workflow string
	Version  string
}

// BundleKey is how a snapshot names this target (I144).
func (t ChildTarget) BundleKey() string {
	if t.Version != "" {
		return t.Workflow + "@" + t.Version
	}
	return t.Workflow
}

// ChildWorkflows lists the literal subworkflows of a definition, in node order.
func ChildWorkflows(def *wfdef.Definition) []ChildTarget {
	var out []ChildTarget
	seen := map[ChildTarget]bool{}
	for i := range def.Nodes {
		n := &def.Nodes[i]
		if n.Type != wfdef.TypeWorkflowExecute && n.Type != wfdef.TypeFlowForeach {
			continue
		}
		name, version := executeTarget(n)
		if name == "" {
			continue
		}
		t := ChildTarget{Workflow: name, Version: version}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
