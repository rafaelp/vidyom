package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

func jobRows(items []domain.Job) [][]string {
	rows := make([][]string, 0, len(items))
	for _, j := range items {
		rows = append(rows, []string{string(j.ID), j.WorkflowID, j.WorkflowVersion, string(j.Status), fmt.Sprintf("%.0f%%", j.ProgressPercent), string(j.AssetID), formatTime(j.CreatedAt)})
	}
	return rows
}

func jobLines(w io.Writer, job api.JobDetail) {
	fields(w,
		kv("id", string(job.ID)), kv("ulid", string(job.ULID)), kv("workflow", job.WorkflowID+" "+job.WorkflowVersion),
		kv("status", string(job.Status)), kv("reason", job.StatusReason), kv("progress", fmt.Sprintf("%.0f%%", job.ProgressPercent)),
		kv("asset", string(job.AssetID)), kv("submitted by", job.SubmittedBy), kv("created", formatTime(job.CreatedAt)),
		kv("started", formatTimePtr(job.StartedAt)), kv("finished", formatTimePtr(job.FinishedAt)),
	)
	if job.Error != nil {
		fields(w, kv("error", job.Error.Code+": "+job.Error.Message), kv("failed node", job.Error.NodeID))
	}
	if job.Attention != nil {
		fields(w, kv("needs attention", job.Attention.NodeID+" ("+job.Attention.NodeType+")"), kv("attention reason", job.Attention.Reason),
			kv("outcome unknown", yesNo(job.Attention.OutcomeUnknown)), kv("since", formatTime(job.Attention.Since)))
		fmt.Fprintf(w, "Check the remote state, then: vidyom job resolve %s --retry-node %s | --skip-node %s | --fail\n",
			clean(string(job.ID)), clean(job.Attention.NodeID), clean(job.Attention.NodeID))
	}
	if len(job.Nodes) > 0 {
		rows := make([][]string, 0, len(job.Nodes))
		for _, n := range job.Nodes {
			rows = append(rows, []string{n.NodeID, n.NodeType, string(n.Status), fmt.Sprintf("%d", n.Attempt), fmt.Sprintf("%.0f%%", n.ProgressPercent)})
		}
		fmt.Fprintln(w)
		table(w, []string{"NODE", "TYPE", "STATUS", "ATTEMPT", "PROGRESS"}, rows)
	}
}

func jobEventLine(w io.Writer, ev domain.JobEvent) {
	parts := []string{ev.At.UTC().Format("15:04:05")}
	if ev.Kind == domain.JobEventLog && ev.Level != "" {
		parts = append(parts, strings.ToUpper(clean(ev.Level)))
	} else {
		parts = append(parts, clean(string(ev.Kind)))
	}
	if ev.NodeID != "" {
		parts = append(parts, clean(ev.NodeID))
	}
	if ev.Message != "" {
		parts = append(parts, clean(ev.Message))
	}
	fmt.Fprintln(w, strings.Join(parts, "  "))
}

// followJob streams a job's events. It issues only the route's GET, so following
// cannot change the job, and Ctrl+C only disconnects (§5.5, §11.5).
func (a *app) followJob(ctx context.Context, ref string, logsOnly bool) error {
	client, err := a.api()
	if err != nil {
		return err
	}
	stream := apiclient.StreamOptions{
		Call: apiclient.Call{Route: api.RouteJobsEvents, Path: apiclient.PathParam("id", ref)},
		OnReconnect: func(cause error, delay time.Duration) {
			a.note("disconnected (%s), reconnecting in %s", errs.Public(cause).Message, delay.Round(100*time.Millisecond))
		},
		OnEvent: func(ev apiclient.Event) error {
			if ev.Name != api.SSEJobEvent {
				return nil
			}
			var decoded domain.JobEvent
			if err := json.Unmarshal(ev.Data, &decoded); err != nil {
				return nil // a frame this version cannot read is not a reason to stop following
			}
			if logsOnly && decoded.Kind != domain.JobEventLog {
				return nil
			}
			if a.jsonOutput {
				var compact bytes.Buffer
				if err := json.Compact(&compact, ev.Data); err != nil {
					return nil
				}
				compact.WriteByte('\n')
				_, err := a.stdout().Write(compact.Bytes())
				return err
			}
			jobEventLine(a.stdout(), decoded)
			return nil
		},
	}
	return client.Stream(ctx, stream)
}

// disconnected explains a Ctrl+C on a stream: the job is not affected.
func (a *app) disconnected(ref string) {
	if a.jsonOutput {
		fmt.Fprintf(a.stderr(), "{\"disconnected\":true,\"job_id\":%q}\n", ref)
	} else {
		fmt.Fprintf(a.stderr(), "\nDisconnected. %s keeps running on the server; reattach with 'vidyom job attach %s'.\n", clean(ref), clean(ref))
	}
	a.exitCode = exitInterrupted
}

func (a *app) jobCommand() *cobra.Command {
	var (
		lf      listFlags
		q       api.JobQuery
		status  string
		follow  bool
		lfLogs  listFlags
		reason  string
		retry   string
		skip    string
		failJob bool
		note    string
	)
	list := &cobra.Command{
		Use:   "list",
		Short: "List jobs",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			q.PageQuery, q.Status = lf.page(), domain.JobStatus(status)
			items, err := fetchList[domain.Job](cmd.Context(), a, apiclient.Call{Route: api.RouteJobsList, Query: apiclient.Query(q)}, &lf)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				table(w, []string{"ID", "WORKFLOW", "VERSION", "STATUS", "PROGRESS", "ASSET", "CREATED"}, jobRows(items.Items))
				a.noteNextPage(items.NextCursor)
			})
		},
	}
	lf.add(list)
	list.Flags().StringVar(&status, "status", "", "queued, running, waiting_approval, retrying, paused, completed, failed, cancelled or needs_attention")
	list.Flags().StringVar(&q.Workflow, "workflow", "", "jobs of this workflow")
	list.Flags().StringVar(&q.AssetID, "asset", "", "jobs of this asset")

	getJob := func(ctx context.Context, ref string) (api.JobDetail, error) {
		return invoke[api.JobDetail](ctx, a, apiclient.Call{Route: api.RouteJobsGet, Path: apiclient.PathParam("id", ref)})
	}
	statusCmd := &cobra.Command{
		Use:   "status JOB-nnn",
		Short: "Show a job's state, nodes and error",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("job", args[0]); err != nil {
				return err
			}
			job, err := getJob(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.emit(job, func(w io.Writer) { jobLines(w, job) })
		},
	}
	attach := &cobra.Command{
		Use:   "attach JOB-nnn",
		Short: "Follow a job until it ends; Ctrl+C disconnects and leaves the job running",
		Long:  "Follows a job's events, reconnecting across lost connections and daemon restarts. This never changes the job. It exits 0 when the job completed, 1 when it failed or was cancelled, and 130 on Ctrl+C, which only disconnects.",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			if err := validateRef("job", ref); err != nil {
				return err
			}
			ctx := cmd.Context()
			job, err := getJob(ctx, ref)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					a.disconnected(ref)
					return nil
				}
				return err
			}
			a.note("Attached to %s (%s), status %s. Ctrl+C disconnects; the job keeps running.", clean(string(job.ID)), clean(job.WorkflowID), clean(string(job.Status)))
			if err := a.followJob(ctx, ref, false); err != nil {
				if errors.Is(err, context.Canceled) {
					a.disconnected(ref)
					return nil
				}
				return err
			}
			final, err := getJob(ctx, ref)
			if err != nil {
				return err
			}
			if final.Status == domain.JobFailed || final.Status == domain.JobCancelled {
				a.exitCode = exitFailed
			}
			if a.jsonOutput {
				return a.writeLine(map[string]any{"end": true, "job_id": final.ID, "status": final.Status, "error": final.Error})
			}
			fields(a.stdout(), kv("job", string(final.ID)), kv("status", string(final.Status)))
			if final.Error != nil {
				fields(a.stdout(), kv("error", final.Error.Code+": "+final.Error.Message))
			}
			return nil
		},
	}
	logs := &cobra.Command{
		Use:   "logs JOB-nnn [--follow]",
		Short: "Show a job's logs; --follow streams them, and Ctrl+C only disconnects",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			if err := validateRef("job", ref); err != nil {
				return err
			}
			ctx := cmd.Context()
			if follow {
				if err := a.followJob(ctx, ref, true); err != nil {
					if errors.Is(err, context.Canceled) {
						a.disconnected(ref)
						return nil
					}
					return err
				}
				return nil
			}
			items, err := fetchList[domain.JobEvent](ctx, a, apiclient.Call{
				Route: api.RouteJobsLogs, Path: apiclient.PathParam("id", ref), Query: apiclient.Query(lfLogs.page()),
			}, &lfLogs)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				for _, ev := range items.Items {
					jobEventLine(w, ev)
				}
				a.noteNextPage(items.NextCursor)
			})
		},
	}
	logs.Flags().BoolVar(&follow, "follow", false, "stream logs until the job ends")
	lfLogs.add(logs)

	action := func(use, short, route, verb string) *cobra.Command {
		c := &cobra.Command{
			Use:   use,
			Short: short,
			Args:  exactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := validateRef("job", args[0]); err != nil {
					return err
				}
				job, err := invoke[domain.Job](cmd.Context(), a, apiclient.Call{
					Route: route, Path: apiclient.PathParam("id", args[0]), Body: api.JobActionRequest{Reason: reason},
				})
				if err != nil {
					return err
				}
				return a.emit(job, func(w io.Writer) {
					fmt.Fprintf(w, "%s %s; status %s.\n", verb, clean(string(job.ID)), clean(string(job.Status)))
				})
			},
		}
		c.Flags().StringVar(&reason, "reason", "", "why; recorded with the action")
		return c
	}
	resolve := &cobra.Command{
		Use:   "resolve JOB-nnn (--retry-node NODE | --skip-node NODE | --fail)",
		Short: "Leave needs_attention after checking the remote state; the only way out",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("job", args[0]); err != nil {
				return err
			}
			req := api.ResolveJobRequest{Note: note}
			chosen := 0
			if retry != "" {
				req.Action, req.NodeID = domain.ResolveRetryNode, retry
				chosen++
			}
			if skip != "" {
				req.Action, req.NodeID = domain.ResolveSkipNode, skip
				chosen++
			}
			if failJob {
				req.Action = domain.ResolveFail
				chosen++
			}
			if chosen != 1 {
				return usagef("give exactly one of --retry-node, --skip-node or --fail")
			}
			job, err := invoke[domain.Job](cmd.Context(), a, apiclient.Call{Route: api.RouteJobsResolve, Path: apiclient.PathParam("id", args[0]), Body: req})
			if err != nil {
				return err
			}
			return a.emit(job, func(w io.Writer) {
				fmt.Fprintf(w, "Resolved %s; status %s.\n", clean(string(job.ID)), clean(string(job.Status)))
			})
		},
	}
	resolve.Flags().StringVar(&retry, "retry-node", "", "run this node again")
	resolve.Flags().StringVar(&skip, "skip-node", "", "continue as if this node had completed")
	resolve.Flags().BoolVar(&failJob, "fail", false, "fail the job")
	resolve.Flags().StringVar(&note, "note", "", "note recorded in the audit")

	return group("job", "Follow and control jobs", list, statusCmd, attach, logs,
		action("retry JOB-nnn", "Retry a failed job from its checkpoints", api.RouteJobsRetry, "Retried"),
		action("pause JOB-nnn", "Pause a job at its next checkpoint", api.RouteJobsPause, "Paused"),
		action("resume JOB-nnn", "Resume a paused job", api.RouteJobsResume, "Resumed"),
		action("cancel JOB-nnn", "Cancel a job; the running node finishes, or aborts if it can", api.RouteJobsCancel, "Cancelled"),
		resolve,
	)
}
