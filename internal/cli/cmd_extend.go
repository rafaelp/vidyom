package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
)

func (a *app) pluginCommand() *cobra.Command {
	list := &cobra.Command{
		Use:   "list",
		Short: "List installed plugins",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := fetchList[domain.PluginRecord](cmd.Context(), a, apiclient.Call{Route: api.RoutePluginsList}, nil)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, p := range items.Items {
					rows = append(rows, []string{p.ID, p.Version, yesNo(p.Enabled), p.SHA256, p.InstalledBy})
				}
				table(w, []string{"ID", "VERSION", "ENABLED", "SHA-256", "INSTALLED BY"}, rows)
			})
		},
	}
	inspect := &cobra.Command{
		Use:   "inspect ID",
		Short: "Show a plugin's manifest and declared permissions",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := invoke[domain.PluginRecord](cmd.Context(), a, apiclient.Call{Route: api.RoutePluginsGet, Path: apiclient.PathParam("id", args[0])})
			if err != nil {
				return err
			}
			return a.emit(p, func(w io.Writer) { pluginLines(w, p) })
		},
	}
	install := &cobra.Command{
		Use:   "install PATH",
		Short: "Install a plugin from a directory on the server",
		Long:  "PATH names a directory on the server: on a Unix socket, the same host, so a relative path resolves here; on a remote server it must be absolute (I178).",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, entry, _, err := a.resolveServer()
			if err != nil {
				return err
			}
			path := args[0]
			if !filepath.IsAbs(path) {
				if entry.Kind != apiclient.KindUnix {
					return usagef("the plugin directory is on the server, so a remote server needs an absolute path")
				}
				if path, err = filepath.Abs(path); err != nil {
					return errs.Wrap(errs.CodeInvalidArgument, err, "cannot resolve the plugin path")
				}
			}
			p, err := invoke[domain.PluginRecord](cmd.Context(), a, apiclient.Call{Route: api.RoutePluginsAdd, Body: api.InstallPluginRequest{Path: path}})
			if err != nil {
				return err
			}
			return a.emit(p, func(w io.Writer) {
				fmt.Fprintf(w, "Installed %s %s.\n", clean(p.ID), clean(p.Version))
				pluginLines(w, p)
			})
		},
	}
	return group("plugin", "Inspect and install plugins", list, inspect, install)
}

func pluginLines(w io.Writer, p domain.PluginRecord) {
	fields(w, kv("id", p.ID), kv("version", p.Version), kv("enabled", yesNo(p.Enabled)), kv("sha256", p.SHA256),
		kv("installed by", p.InstalledBy), kv("installed", formatTime(p.InstalledAt)))
	if len(p.Manifest) > 0 {
		fmt.Fprintf(w, "manifest:\n%s\n", prettyJSON(p.Manifest))
	}
}

func (a *app) hookCommand() *cobra.Command {
	var (
		workflow string
		asset    string
	)
	list := &cobra.Command{
		Use:   "list [--workflow ID]",
		Short: "List hooks at global, workflow and node level",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var query url.Values
			if workflow != "" {
				query = url.Values{"workflow": {workflow}}
			}
			items, err := fetchList[api.HookInfo](cmd.Context(), a, apiclient.Call{Route: api.RouteHooksList, Query: query}, nil)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, h := range items.Items {
					rows = append(rows, []string{h.ID, h.Point, h.Level, h.Plugin, h.Action})
				}
				table(w, []string{"ID", "POINT", "LEVEL", "PLUGIN", "ACTION"}, rows)
			})
		},
	}
	list.Flags().StringVar(&workflow, "workflow", "", "hooks of this workflow")
	test := &cobra.Command{
		Use:   "test ID [--asset ID]",
		Short: "Run one hook on demand",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if asset != "" {
				if err := validateRef("asset", asset); err != nil {
					return err
				}
			}
			result, err := invoke[map[string]any](cmd.Context(), a, apiclient.Call{
				Route: api.RouteHooksTest, Path: apiclient.PathParam("id", args[0]), Body: api.TestHookRequest{AssetID: asset},
			})
			if err != nil {
				return err
			}
			return a.emit(result, func(w io.Writer) {
				raw, _ := json.Marshal(result)
				fmt.Fprintln(w, prettyJSON(raw))
			})
		},
	}
	test.Flags().StringVar(&asset, "asset", "", "asset passed to the hook")
	return group("hook", "Inspect and test lifecycle hooks", list, test)
}

func (a *app) eventCommand() *cobra.Command {
	var (
		lf, deliveryFlags listFlags
		since, eventType  string
		watchType         string
		watchSince        string
		deliveryStatus    string
	)
	list := &cobra.Command{
		Use:   "list [--since DURATION] [--type TYPE]",
		Short: "List the tenant's events",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			parsedSince, err := parseSince(since, a.opts.Now())
			if err != nil {
				return usagef("%v", err)
			}
			q := api.EventQuery{PageQuery: lf.page(), Since: parsedSince, Type: event.Type(eventType)}
			items, err := fetchList[event.Envelope](cmd.Context(), a, apiclient.Call{Route: api.RouteEventsList, Query: apiclient.Query(q)}, &lf)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, e := range items.Items {
					rows = append(rows, []string{formatTime(e.OccurredAt), string(e.Type), e.AssetID, e.JobID, string(e.EventID)})
				}
				table(w, []string{"TIME", "TYPE", "ASSET", "JOB", "EVENT"}, rows)
				a.noteNextPage(items.NextCursor)
			})
		},
	}
	lf.add(list)
	list.Flags().StringVar(&since, "since", "", "events since a duration ago (24h, 7d) or an RFC 3339 time")
	list.Flags().StringVar(&eventType, "type", "", "only this event type")

	watch := &cobra.Command{
		Use:   "watch [--type TYPE]",
		Short: "Stream the tenant's events until Ctrl+C",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			parsedSince, err := parseSince(watchSince, a.opts.Now())
			if err != nil {
				return usagef("%v", err)
			}
			client, err := a.api()
			if err != nil {
				return err
			}
			err = client.Stream(ctx, apiclient.StreamOptions{
				Call:        apiclient.Call{Route: api.RouteEventsStream, Query: apiclient.Query(api.EventQuery{Since: parsedSince, Type: event.Type(watchType)})},
				OnReconnect: a.noteReconnect,
				OnEvent: func(ev apiclient.Event) error {
					if ev.Name != api.SSEEvent {
						return nil
					}
					var envelope event.Envelope
					if err := json.Unmarshal(ev.Data, &envelope); err != nil {
						return nil
					}
					if a.jsonOutput {
						return a.writeLine(envelope)
					}
					fmt.Fprintf(a.stdout(), "%s  %s  %s  %s\n", formatTime(envelope.OccurredAt), clean(string(envelope.Type)), clean(envelope.AssetID), clean(envelope.JobID))
					return nil
				},
			})
			return a.streamEnded(ctx, err)
		},
	}
	watch.Flags().StringVar(&watchType, "type", "", "only this event type")
	watch.Flags().StringVar(&watchSince, "since", "", "start from a duration ago (24h, 7d) or an RFC 3339 time")

	deliveries := &cobra.Command{
		Use:   "deliveries [--status STATUS]",
		Short: "Show event deliveries to workflows and plugins",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := api.DeliveryQuery{PageQuery: deliveryFlags.page(), Status: event.DeliveryStatus(deliveryStatus)}
			items, err := fetchList[event.Delivery](cmd.Context(), a, apiclient.Call{Route: api.RouteEventsDeliv, Query: apiclient.Query(q)}, &deliveryFlags)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, d := range items.Items {
					rows = append(rows, []string{string(d.EventID), d.Subscriber, string(d.Status), fmt.Sprint(d.Attempts), d.LastError})
				}
				table(w, []string{"EVENT", "SUBSCRIBER", "STATUS", "ATTEMPTS", "LAST ERROR"}, rows)
				a.noteNextPage(items.NextCursor)
			})
		},
	}
	deliveryFlags.add(deliveries)
	deliveries.Flags().StringVar(&deliveryStatus, "status", "", "only this delivery status")
	return group("event", "Inspect and follow the Event Bus", list, watch, deliveries)
}
