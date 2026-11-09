package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

func doctorStatusFailed(checks []api.DoctorCheck) bool {
	for _, check := range checks {
		if check.Status == "fail" {
			return true
		}
	}
	return false
}

func doctorTable(w io.Writer, checks []api.DoctorCheck) {
	rows := make([][]string, 0, len(checks))
	for _, check := range checks {
		rows = append(rows, []string{check.Name, check.Status, check.Detail})
	}
	table(w, []string{"CHECK", "STATUS", "DETAIL"}, rows)
}

// clientChecks are what only this machine can answer: which transport it trusts and
// whether its keyring works. They read the keyring and never write to it.
func (a *app) clientChecks(wantSecurity bool) []api.DoctorCheck {
	var checks []api.DoctorCheck
	name, entry, _, err := a.resolveServer()
	if err != nil {
		return []api.DoctorCheck{{Name: "client.server", Status: "fail", Detail: errs.Public(err).Message}}
	}
	transport := api.DoctorCheck{Name: "client.transport", Status: "ok", Detail: entry.endpoint().Display()}
	switch {
	case entry.Kind == apiclient.KindHTTPS && entry.Fingerprint != "":
		transport.Detail += ", TLS 1.3, certificate pinned"
	case entry.Kind == apiclient.KindHTTPS:
		transport.Detail += ", TLS 1.3, certificate validated"
	}
	checks = append(checks, transport)
	if !wantSecurity {
		return checks
	}
	credential := api.DoctorCheck{Name: "client.keyring", Status: "ok"}
	_, keyErr := a.keyring().Get(keyringService, deviceAccount(name))
	switch {
	case keyErr == nil:
		credential.Detail = "this device's credential is in the OS keyring"
	case errors.Is(keyErr, security.ErrKeyringNotFound):
		if entry.Kind == apiclient.KindUnix {
			credential.Detail = "keyring usable; no credential stored, using the socket's peer identity"
		} else {
			credential.Status = "fail"
			credential.Detail = "keyring usable, but this client is not paired with " + name
		}
	default:
		if entry.Kind == apiclient.KindUnix {
			credential.Status = "warn"
			credential.Detail = "no usable OS keyring; the socket's peer identity still works"
		} else {
			credential.Status = "fail"
			credential.Detail = security.ErrKeyringUnavailable.Error()
		}
	}
	return append(checks, credential)
}

func (a *app) doctorCommand() *cobra.Command {
	var securityFlag, networkFlag, storageFlag bool
	cmd := &cobra.Command{
		Use:   "doctor [--security] [--network] [--storage]",
		Short: "Check the daemon and this client; exits non-zero when a check fails",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			var requests []url.Values
			if securityFlag {
				requests = append(requests, url.Values{"check": {"security"}})
			}
			if networkFlag {
				requests = append(requests, url.Values{"check": {"network"}})
			}
			if storageFlag {
				requests = append(requests, url.Values{"check": {"storage"}})
			}
			if len(requests) == 0 {
				requests = append(requests, nil)
			}
			wantClient := securityFlag || networkFlag || !storageFlag
			out := api.DoctorResponse{Checks: []api.DoctorCheck{}}
			if wantClient {
				out.Checks = append(out.Checks, a.clientChecks(securityFlag || !networkFlag)...)
			}
			for _, query := range requests {
				resp, err := invoke[api.DoctorResponse](ctx, a, apiclient.Call{Route: api.RouteDoctor, Query: query})
				if errs.IsCode(err, errs.CodeUnavailable) {
					// A stopped daemon is exactly what a doctor must be able to report.
					out.Checks = append(out.Checks, api.DoctorCheck{Name: "client.daemon", Status: "fail", Detail: errs.Public(err).Message})
					break
				}
				if err != nil {
					return err
				}
				out.Checks = append(out.Checks, resp.Checks...)
			}
			if doctorStatusFailed(out.Checks) {
				a.exitCode = exitFailed
			}
			return a.emit(out, func(w io.Writer) { doctorTable(w, out.Checks) })
		},
	}
	cmd.Flags().BoolVar(&securityFlag, "security", false, "sealing level, socket and data modes, TLS, this client's keyring")
	cmd.Flags().BoolVar(&networkFlag, "network", false, "listeners, interfaces, Redis and the AI worker")
	cmd.Flags().BoolVar(&storageFlag, "storage", false, "the tenant's storage roles")
	return cmd
}

func (a *app) securityCommand() *cobra.Command {
	var (
		q                 api.AuditQuery
		since             string
		verify, export    bool
		lf, sessionsFlags listFlags
	)
	status := &cobra.Command{
		Use:   "status",
		Short: "Show how the master key is sealed and the daemon's security checks",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			health, err := a.health(ctx)
			if err != nil {
				return err
			}
			report, err := invoke[api.DoctorResponse](ctx, a, apiclient.Call{Route: api.RouteDoctor, Query: url.Values{"check": {"security"}}})
			if err != nil {
				return err
			}
			if report.Checks == nil {
				report.Checks = []api.DoctorCheck{}
			}
			out := struct {
				Status  string            `json:"status"`
				Sealing string            `json:"sealing,omitempty"`
				Checks  []api.DoctorCheck `json:"checks"`
			}{health.Status, health.Sealing, report.Checks}
			if doctorStatusFailed(report.Checks) {
				a.exitCode = exitFailed
			}
			return a.emit(out, func(w io.Writer) {
				fields(w, kv("status", out.Status), kv("sealing", out.Sealing))
				doctorTable(w, out.Checks)
			})
		},
	}
	audit := &cobra.Command{
		Use:   "audit [--since DURATION] [--verify] [--export]",
		Short: "List the audit trail, verify its hash chain or export it",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if verify && export {
				return usagef("give --verify or --export, not both")
			}
			if verify {
				result, err := invoke[domain.AuditVerification](ctx, a, apiclient.Call{Route: api.RouteAuditVerify, NoTimeout: true})
				if err != nil {
					return err
				}
				if !result.OK {
					a.exitCode = exitFailed
				}
				return a.emit(result, func(w io.Writer) {
					if result.OK {
						fmt.Fprintf(w, "The hash chain is intact: %d records verified.\n", result.Records)
						return
					}
					fmt.Fprintf(w, "The hash chain is BROKEN at record %d: %s\n", result.FirstBadSeq, clean(result.Reason))
				})
			}
			if export {
				result, err := invoke[api.AuditExportResponse](ctx, a, apiclient.Call{Route: api.RouteAuditExport, Body: struct{}{}, NoTimeout: true})
				if err != nil {
					return err
				}
				return a.emit(result, func(w io.Writer) { fmt.Fprintf(w, "Exported to archive: %s\n", clean(result.Key)) })
			}
			parsedSince, err := parseSince(since, a.opts.Now())
			if err != nil {
				return usagef("%v", err)
			}
			q.PageQuery, q.Since = lf.page(), parsedSince
			items, err := fetchList[domain.AuditRecord](ctx, a, apiclient.Call{Route: api.RouteAuditList, Query: apiclient.Query(q)}, &lf)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, r := range items.Items {
					rows = append(rows, []string{formatTime(r.At), r.Actor, r.Action, string(r.Permission), r.Resource, r.Result})
				}
				table(w, []string{"TIME", "ACTOR", "ACTION", "PERMISSION", "RESOURCE", "RESULT"}, rows)
				a.noteNextPage(items.NextCursor)
			})
		},
	}
	lf.add(audit)
	audit.Flags().StringVar(&since, "since", "", "records since a duration ago (24h, 7d) or an RFC 3339 time")
	audit.Flags().StringVar(&q.Actor, "actor", "", "only this actor")
	audit.Flags().StringVar(&q.Action, "action", "", "only this action")
	audit.Flags().StringVar(&q.Resource, "resource", "", "only this resource")
	audit.Flags().BoolVar(&verify, "verify", false, "verify the hash chain; exits non-zero when it is broken")
	audit.Flags().BoolVar(&export, "export", false, "export the chain since the last export to archive")

	sessions := &cobra.Command{
		Use:   "sessions",
		Short: "List paired clients",
		Args:  noArgs(),
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.listDevices(cmd, &sessionsFlags) },
	}
	sessionsFlags.add(sessions)
	revoke := &cobra.Command{
		Use:   "revoke CLIENT-nnn",
		Short: "Revoke a paired client",
		Args:  exactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.revokeDevice(cmd, args[0]) },
	}
	return group("security", "Audit and client sessions", status, audit, sessions, revoke)
}

func logLine(w io.Writer, record connector.LogRecord) {
	parts := []string{formatTime(record.Timestamp), strings.ToUpper(clean(record.Level)), clean(record.Service), clean(record.Event)}
	if record.JobID != "" {
		parts = append(parts, clean(record.JobID))
	}
	if record.Message != "" {
		parts = append(parts, clean(record.Message))
	}
	fmt.Fprintln(w, strings.Join(parts, "  "))
}

func (a *app) logsCommand() *cobra.Command {
	var (
		q      api.LogQuery
		since  string
		follow bool
		lf     listFlags
	)
	cmd := &cobra.Command{
		Use:   "logs [--follow]",
		Short: "Show the tenant's logs; --follow streams them, and Ctrl+C only disconnects",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			parsedSince, err := parseSince(since, a.opts.Now())
			if err != nil {
				return usagef("%v", err)
			}
			q.PageQuery, q.Since = lf.page(), parsedSince
			if follow {
				client, err := a.api()
				if err != nil {
					return err
				}
				err = client.Stream(ctx, apiclient.StreamOptions{
					Call:        apiclient.Call{Route: api.RouteLogsStream, Query: apiclient.Query(q)},
					OnReconnect: a.noteReconnect,
					OnEvent: func(ev apiclient.Event) error {
						if ev.Name != api.SSELog {
							return nil
						}
						var record connector.LogRecord
						if err := json.Unmarshal(ev.Data, &record); err != nil {
							return nil
						}
						if a.jsonOutput {
							return a.writeLine(record)
						}
						logLine(a.stdout(), record)
						return nil
					},
				})
				return a.streamEnded(ctx, err)
			}
			items, err := fetchList[connector.LogRecord](ctx, a, apiclient.Call{Route: api.RouteLogsList, Query: apiclient.Query(q)}, &lf)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				for _, record := range items.Items {
					logLine(w, record)
				}
				a.noteNextPage(items.NextCursor)
			})
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "stream new records until Ctrl+C")
	cmd.Flags().StringVar(&since, "since", "", "records since a duration ago (24h, 7d) or an RFC 3339 time")
	cmd.Flags().StringVar(&q.Level, "level", "", "minimum level")
	cmd.Flags().StringVar(&q.JobID, "job", "", "only this job")
	lf.add(cmd)
	return cmd
}

func (a *app) noteReconnect(cause error, delay time.Duration) {
	a.note("disconnected (%s), reconnecting in %s", errs.Public(cause).Message, delay.Round(100*time.Millisecond))
}

// streamEnded turns a stream's end into the command's result: Ctrl+C on a stream that
// has no job is still a clean disconnect (I174).
func (a *app) streamEnded(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		a.note("\nDisconnected.")
		a.exitCode = exitInterrupted
		return nil
	}
	return err
}

func (a *app) observabilityCommand() *cobra.Command {
	sinksTable := func(w io.Writer, items []api.ObservabilitySink) {
		rows := make([][]string, 0, len(items))
		for _, s := range items {
			rows = append(rows, []string{s.ID, s.Kind, s.Driver, yesNo(s.Enabled), s.Status, fmt.Sprint(s.Backlog), fmt.Sprint(s.Dropped), s.LastError})
		}
		table(w, []string{"ID", "KIND", "DRIVER", "ENABLED", "STATUS", "BACKLOG", "DROPPED", "LAST ERROR"}, rows)
	}
	listing := func(use, short, route string) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  noArgs(),
			RunE: func(cmd *cobra.Command, _ []string) error {
				items, err := fetchList[api.ObservabilitySink](cmd.Context(), a, apiclient.Call{Route: route}, nil)
				if err != nil {
					return err
				}
				return a.emit(items, func(w io.Writer) { sinksTable(w, items.Items) })
			},
		}
	}
	action := func(use, short, route, verb string) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  exactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if _, err := invoke[struct{}](cmd.Context(), a, apiclient.Call{Route: route, Path: apiclient.PathParam("id", args[0]), Body: struct{}{}}); err != nil {
					return err
				}
				return a.emit(map[string]string{"sink": args[0], "result": verb}, func(w io.Writer) { fmt.Fprintf(w, "%s: %s.\n", clean(args[0]), verb) })
			},
		}
	}
	return group("observability", "Inspect and control log, metric and trace exporters",
		listing("list", "List configured exporters", api.RouteObsList),
		action("test ID", "Send a test record through an exporter", api.RouteObsTest, "test passed"),
		action("enable ID", "Enable an exporter", api.RouteObsEnable, "enabled"),
		action("disable ID", "Disable an exporter", api.RouteObsDisable, "disabled"),
		listing("status", "Show exporters' state, backlog and drops", api.RouteObsStatus),
	)
}
