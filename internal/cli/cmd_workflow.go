package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// isWorkflowFile tells a file from a workflow ID. IDs have no separators or .json, so
// a local file that happens to share an ID's name never changes what runs.
func isWorkflowFile(target string) bool {
	return strings.ContainsAny(target, `/\`) || strings.HasSuffix(strings.ToLower(target), ".json")
}

// readWorkflowFile reads a definition and the inputs it declares. It only checks that
// the file is JSON: validation is the daemon's (§6.2).
func readWorkflowFile(path string) (json.RawMessage, map[string]wfdef.InputSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, errs.Wrap(errs.CodeInvalidArgument, err, "cannot read the workflow file")
	}
	var shape struct {
		Inputs map[string]wfdef.InputSpec `json:"inputs"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		return nil, nil, errs.Wrap(errs.CodeInvalidArgument, err, "the workflow file is not valid JSON")
	}
	return raw, shape.Inputs, nil
}

func (a *app) runRegisteredWorkflow(cmd *cobra.Command, workflow, version string, inputs map[string]any) error {
	encoded, err := json.Marshal(inputs)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot encode the workflow inputs")
	}
	resp, err := invoke[api.RunWorkflowResponse](cmd.Context(), a, apiclient.Call{
		Route: api.RouteWorkflowsRun, Path: apiclient.PathParam("id", workflow),
		Body: api.RunWorkflowRequest{WorkflowVersion: version, Inputs: encoded},
	})
	if err != nil {
		return err
	}
	return a.emitRun(resp)
}

func issueLines(w io.Writer, issues []api.ValidationIssue) {
	for _, issue := range issues {
		where := issue.NodeID
		if where == "" {
			where = issue.Path
		}
		if where != "" {
			where = " (" + clean(where) + ")"
		}
		fmt.Fprintf(w, "%s: %s%s: %s\n", clean(issue.Severity), clean(issue.Code), where, clean(issue.Message))
	}
}

func (a *app) emitValidation(result api.ValidationResult) error {
	if !result.Valid {
		a.exitCode = exitFailed
	}
	return a.emit(result, func(w io.Writer) {
		if result.Valid {
			fmt.Fprintln(w, "The workflow is valid.")
		} else {
			fmt.Fprintln(w, "The workflow is not valid.")
		}
		issueLines(w, result.Issues)
	})
}

func (a *app) workflowCommand() *cobra.Command {
	var (
		lf          listFlags
		showVersion string
		runVersion  string
		inputs      []string
		dryRun      bool
	)
	list := &cobra.Command{
		Use:   "list",
		Short: "List the workflows you may run",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := fetchList[api.WorkflowSummary](cmd.Context(), a, apiclient.Call{Route: api.RouteWorkflowsList, Query: apiclient.Query(lf.page())}, &lf)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, wf := range items.Items {
					trigger := ""
					if wf.Trigger != nil {
						trigger = string(wf.Trigger.Type)
					}
					rows = append(rows, []string{wf.ID, wf.Version, trigger, wf.Name})
				}
				table(w, []string{"ID", "VERSION", "TRIGGER", "NAME"}, rows)
				a.noteNextPage(items.NextCursor)
			})
		},
	}
	lf.add(list)
	show := &cobra.Command{
		Use:   "show ID",
		Short: "Show a workflow's definition",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			detail, err := invoke[api.WorkflowDetail](cmd.Context(), a, apiclient.Call{
				Route: api.RouteWorkflowsGet, Path: apiclient.PathParam("id", args[0]), Query: versionQuery(showVersion),
			})
			if err != nil {
				return err
			}
			return a.emit(detail, func(w io.Writer) {
				fields(w, kv("id", detail.ID), kv("name", detail.Name), kv("version", detail.Version), kv("sha256", detail.SHA256), kv("description", detail.Description))
				names := make([]string, 0, len(detail.Inputs))
				for name := range detail.Inputs {
					names = append(names, name)
				}
				sort.Strings(names)
				rows := make([][]string, 0, len(names))
				for _, name := range names {
					spec := detail.Inputs[name]
					rows = append(rows, []string{name, string(spec.Type), yesNo(spec.Required), spec.Description})
				}
				if len(rows) > 0 {
					table(w, []string{"INPUT", "TYPE", "REQUIRED", "DESCRIPTION"}, rows)
				}
				if len(detail.Definition) > 0 {
					fmt.Fprintf(w, "definition:\n%s\n", prettyJSON(detail.Definition))
				}
			})
		},
	}
	show.Flags().StringVar(&showVersion, "version", "", "workflow version (default: latest)")

	validate := &cobra.Command{
		Use:   "validate FILE",
		Short: "Validate a workflow file on the daemon; exits non-zero when it is not valid",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, _, err := readWorkflowFile(args[0])
			if err != nil {
				return err
			}
			result, err := invoke[api.ValidationResult](cmd.Context(), a, apiclient.Call{
				Route: api.RouteWorkflowsValidate, Body: api.ValidateWorkflowRequest{Definition: raw},
			})
			if err != nil {
				return err
			}
			return a.emitValidation(result)
		},
	}
	run := &cobra.Command{
		Use:   "run (ID|FILE) [--input key=value]... [--dry-run]",
		Short: "Run a workflow, or check it with --dry-run; returns as soon as the job exists",
		Long:  "Runs a registered workflow by ID, or a workflow file. A file input takes server:, upload:, storage:// or import: references; a bare absolute path means server: (R20).",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target := args[0]
			req := api.RunWorkflowRequest{WorkflowVersion: runVersion, DryRun: dryRun}
			call := apiclient.Call{Route: api.RouteWorkflowsRun}
			var specs map[string]wfdef.InputSpec
			if isWorkflowFile(target) {
				raw, declared, err := readWorkflowFile(target)
				if err != nil {
					return err
				}
				req.Definition, specs = raw, declared
				call.Route = api.RouteWorkflowsRunBody
			} else {
				declared, err := a.workflowSpecs(ctx, target, runVersion)
				if err != nil {
					return err
				}
				specs = declared
				call.Path = apiclient.PathParam("id", target)
			}
			values, err := parseInputs(inputs, specs)
			if err != nil {
				return err
			}
			if len(values) > 0 {
				encoded, err := json.Marshal(values)
				if err != nil {
					return errs.Wrap(errs.CodeInternal, err, "cannot encode the workflow inputs")
				}
				req.Inputs = encoded
			}
			call.Body = req
			resp, err := invoke[api.RunWorkflowResponse](ctx, a, call)
			if err != nil {
				return err
			}
			if resp.Validation != nil {
				if !resp.Validation.Valid {
					a.exitCode = exitFailed
				}
				return a.emit(resp, func(w io.Writer) {
					if resp.Validation.Valid {
						fmt.Fprintln(w, "Dry run: the workflow is valid. Nothing was started.")
					} else {
						fmt.Fprintln(w, "Dry run: the workflow is not valid. Nothing was started.")
					}
					issueLines(w, resp.Validation.Issues)
				})
			}
			return a.emitRun(resp)
		},
	}
	run.Flags().StringArrayVar(&inputs, "input", nil, "workflow input, key=value; repeatable")
	run.Flags().BoolVar(&dryRun, "dry-run", false, "validate only; warns about client credentials in autonomous workflows (R37)")
	run.Flags().StringVar(&runVersion, "version", "", "workflow version (default: latest)")

	return group("workflow", "List, validate and run workflows", list, show, validate, run)
}

func (a *app) nodeCommand() *cobra.Command {
	var (
		req        api.RunNodeRequest
		params     []string
		paramsJSON string
	)
	list := &cobra.Command{
		Use:   "list",
		Short: "List node types",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := fetchList[node.Descriptor](cmd.Context(), a, apiclient.Call{Route: api.RouteNodesList}, nil)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, n := range items.Items {
					rows = append(rows, []string{n.Type, strconv.Itoa(n.TypeVersion), string(n.Pool), string(n.SideEffect), yesNo(n.Paid), n.Title})
				}
				table(w, []string{"TYPE", "VERSION", "POOL", "SIDE EFFECT", "PAID", "TITLE"}, rows)
			})
		},
	}
	describe := &cobra.Command{
		Use:   "describe TYPE",
		Short: "Show a node type's contract",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := invoke[node.Descriptor](cmd.Context(), a, apiclient.Call{Route: api.RouteNodesGet, Path: apiclient.PathParam("type", args[0])})
			if err != nil {
				return err
			}
			return a.emit(n, func(w io.Writer) {
				fields(w, kv("type", n.Type), kv("version", strconv.Itoa(n.TypeVersion)), kv("title", n.Title), kv("owner", n.Owner),
					kv("pool", string(n.Pool)), kv("side effect", string(n.SideEffect)), kv("supports abort", yesNo(n.SupportsAbort)), kv("paid", yesNo(n.Paid)))
				if len(n.Parameters) > 0 {
					fmt.Fprintf(w, "parameters:\n%s\n", prettyJSON(n.Parameters))
				}
				if len(n.Output) > 0 {
					fmt.Fprintf(w, "output:\n%s\n", prettyJSON(n.Output))
				}
			})
		},
	}
	run := &cobra.Command{
		Use:   "run TYPE [--asset ID] [--param key=value]... [--params-json JSON]",
		Short: "Run one node as an ad-hoc job",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if req.AssetID != "" {
				if err := validateRef("asset", req.AssetID); err != nil {
					return err
				}
			}
			encoded, err := nodeParameters(params, paramsJSON)
			if err != nil {
				return err
			}
			req.Parameters = encoded
			resp, err := invoke[api.RunWorkflowResponse](cmd.Context(), a, apiclient.Call{
				Route: api.RouteNodesRun, Path: apiclient.PathParam("type", args[0]), Body: req,
			})
			if err != nil {
				return err
			}
			return a.emitRun(resp)
		},
	}
	run.Flags().StringVar(&req.AssetID, "asset", "", "asset the node works on")
	run.Flags().StringArrayVar(&params, "param", nil, "string parameter, key=value; repeatable")
	run.Flags().StringVar(&paramsJSON, "params-json", "", "parameters as a JSON object, for typed values")
	return group("node", "Inspect and run node types", list, describe, run)
}

func nodeParameters(params []string, raw string) (json.RawMessage, error) {
	values := map[string]any{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return nil, usagef("invalid --params-json: expected a JSON object")
		}
	}
	for _, param := range params {
		key, value, ok := strings.Cut(param, "=")
		if !ok || key == "" {
			return nil, usagef("invalid --param %q: expected key=value", param)
		}
		values[key] = value
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot encode the parameters")
	}
	return encoded, nil
}
