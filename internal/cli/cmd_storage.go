package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
)

func (a *app) connectorCommand() *cobra.Command {
	list := &cobra.Command{
		Use:   "list",
		Short: "List connector drivers and whether each is available",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := fetchList[connector.Info](cmd.Context(), a, apiclient.Call{Route: api.RouteConnectors}, nil)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, c := range items.Items {
					rows = append(rows, []string{string(c.Family), c.Driver, c.Version, yesNo(c.Available), c.Reason})
				}
				table(w, []string{"FAMILY", "DRIVER", "VERSION", "AVAILABLE", "REASON"}, rows)
			})
		},
	}
	return group("connector", "Inspect connectors", list)
}

func (a *app) storageCommand() *cobra.Command {
	var (
		verifyAsset             string
		copyReq                 api.StorageCopyRequest
		from, to                string
		copyAsset               string
		restoreAsset, restoreTo string
		yes                     bool
	)
	list := &cobra.Command{
		Use:   "list",
		Short: "List the tenant's storage roles",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := fetchList[api.StorageInfo](cmd.Context(), a, apiclient.Call{Route: api.RouteStoragesList}, nil)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, s := range items.Items {
					rows = append(rows, []string{string(s.Role), s.Driver, s.Usage})
				}
				table(w, []string{"ROLE", "DRIVER", "USAGE"}, rows)
			})
		},
	}
	test := &cobra.Command{
		Use:   "test ROLE",
		Short: "Check that a storage role is reachable; exits non-zero when it is not",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := invoke[api.StorageTestResponse](cmd.Context(), a, apiclient.Call{
				Route: api.RouteStoragesTest, Path: apiclient.PathParam("role", args[0]), Body: struct{}{}, NoTimeout: true,
			})
			if err != nil {
				return err
			}
			if !resp.OK {
				a.exitCode = exitFailed
			}
			return a.emit(resp, func(w io.Writer) { fields(w, kv("ok", yesNo(resp.OK)), kv("detail", resp.Detail)) })
		},
	}
	verify := &cobra.Command{
		Use:   "verify ROLE --asset ID",
		Short: "Verify an asset's objects in a role by checksum; exits non-zero on a mismatch",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("asset", verifyAsset); err != nil {
				return err
			}
			resp, err := invoke[api.StorageVerifyResponse](cmd.Context(), a, apiclient.Call{
				Route: api.RouteStoragesVerify, Path: apiclient.PathParam("role", args[0]),
				Body: api.StorageVerifyRequest{AssetID: verifyAsset}, NoTimeout: true,
			})
			if err != nil {
				return err
			}
			if !resp.OK {
				a.exitCode = exitFailed
			}
			return a.emit(resp, func(w io.Writer) {
				fields(w, kv("ok", yesNo(resp.OK)), kv("checked", fmt.Sprint(resp.Checked)), kv("failed", strings.Join(resp.Failed, ", ")))
			})
		},
	}
	verify.Flags().StringVar(&verifyAsset, "asset", "", "asset to verify")
	copyCmd := &cobra.Command{
		Use:   "copy --from ROLE --to STORAGE --asset ID",
		Short: "Copy an asset between storage instances; starts a job",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" || to == "" {
				return usagef("--from and --to are required")
			}
			if err := validateRef("asset", copyAsset); err != nil {
				return err
			}
			copyReq = api.StorageCopyRequest{From: domain.StorageRole(from), To: domain.StorageRole(to), AssetID: copyAsset}
			resp, err := invoke[api.RunWorkflowResponse](cmd.Context(), a, apiclient.Call{Route: api.RouteStoragesCopy, Body: copyReq})
			if err != nil {
				return err
			}
			return a.emitRun(resp)
		},
	}
	copyCmd.Flags().StringVar(&from, "from", "", "source role")
	copyCmd.Flags().StringVar(&to, "to", "", "destination")
	copyCmd.Flags().StringVar(&copyAsset, "asset", "", "asset to copy")

	restore := &cobra.Command{
		Use:   "restore --asset ID --to ROLE",
		Short: "Restore from archive (admin): shows the cost, asks, then starts a job",
		Long:  "An explicit administrative operation with the storage.archive.restore permission. The first request starts nothing and returns the cost; only a confirmation, or --yes, starts the restore. No workflow restores from archive by itself.",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if restoreTo == "" {
				return usagef("--to is required")
			}
			if err := validateRef("asset", restoreAsset); err != nil {
				return err
			}
			client, err := a.api()
			if err != nil {
				return err
			}
			request := func(confirm bool, suffix string) (json.RawMessage, error) {
				return apiclient.Invoke[json.RawMessage](ctx, client, apiclient.Call{
					Route:          api.RouteStoragesRestore,
					Body:           api.StorageRestoreRequest{AssetID: restoreAsset, To: domain.StorageRole(restoreTo), Confirm: confirm},
					IdempotencyKey: a.keyFor("restore", suffix),
				})
			}
			first, err := request(false, "preview")
			if err != nil {
				return err
			}
			var preview api.StorageRestorePreview
			if err := json.Unmarshal(first, &preview); err != nil {
				return errs.Wrap(errs.CodeInternal, err, "cannot read the restore preview")
			}
			if !preview.ConfirmRequired {
				// The server started it without waiting; report what it answered.
				var job api.RunWorkflowResponse
				if err := json.Unmarshal(first, &job); err != nil {
					return errs.Wrap(errs.CodeInternal, err, "cannot read the restore answer")
				}
				return a.emitRun(job)
			}
			a.showRestorePreview(preview)
			if err := a.confirm(fmt.Sprintf("Restore %s from archive to %s?", restoreAsset, restoreTo), yes); err != nil {
				if a.jsonOutput {
					// A script sees the cost before deciding.
					_ = a.writeJSON(struct {
						Preview api.StorageRestorePreview `json:"preview"`
					}{preview})
				}
				return err
			}
			second, err := request(true, "confirm")
			if err != nil {
				return err
			}
			var job api.RunWorkflowResponse
			if err := json.Unmarshal(second, &job); err != nil {
				return errs.Wrap(errs.CodeInternal, err, "cannot read the restore answer")
			}
			if a.jsonOutput {
				return a.writeJSON(struct {
					Preview api.StorageRestorePreview `json:"preview"`
					Job     api.RunWorkflowResponse   `json:"job"`
				}{preview, job})
			}
			runLines(a.stdout(), job)
			return nil
		},
	}
	restore.Flags().StringVar(&restoreAsset, "asset", "", "asset to restore")
	restore.Flags().StringVar(&restoreTo, "to", "", "destination role")
	restore.Flags().BoolVar(&yes, "yes", false, "confirm without asking; the cost is still shown")

	return group("storage", "Inspect and operate the tenant's storage roles", list, test, verify, copyCmd, restore)
}

func (a *app) showRestorePreview(preview api.StorageRestorePreview) {
	if a.jsonOutput {
		return
	}
	estimate := "not available: no cost table entry"
	if preview.EstimateMinor != nil {
		estimate = formatMoney(*preview.EstimateMinor, preview.Currency) + " (estimate)"
	}
	fields(a.stderr(),
		kv("asset", string(preview.AssetID)), kv("to", string(preview.To)),
		kv("archive bytes", formatBytes(preview.ArchiveBytes)), kv("cost", estimate), kv("note", preview.Note),
	)
}

// ---- Publishing ----

func (a *app) publishingCommand() *cobra.Command {
	list := &cobra.Command{
		Use:   "list",
		Short: "List publishing destinations",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := fetchList[api.DestinationInfo](cmd.Context(), a, apiclient.Call{Route: api.RouteDestinationsList}, nil)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, d := range items.Items {
					rows = append(rows, []string{d.Name, d.Driver, string(d.Visibility), yesNo(d.Available), d.Reason})
				}
				table(w, []string{"NAME", "DRIVER", "VISIBILITY", "AVAILABLE", "REASON"}, rows)
			})
		},
	}
	return group("publishing", "Inspect publishing destinations", list)
}

func publicationTable(w io.Writer, items []domain.Publication) {
	rows := make([][]string, 0, len(items))
	for _, p := range items {
		rows = append(rows, []string{string(p.ID), string(p.AssetID), p.Destination, string(p.Status), string(p.Visibility), p.AssetVersion, p.URL})
	}
	table(w, []string{"ID", "ASSET", "DESTINATION", "STATUS", "VISIBILITY", "VERSION", "URL"}, rows)
}

func isPublicationRef(ref string) bool {
	return id.Friendly(ref).Prefix() == id.PrefixPublication
}

func (a *app) publishCommand() *cobra.Command {
	var (
		to         []string
		visibility string
		lf         listFlags
		destFilter string
		pubStatus  string
	)
	publish := &cobra.Command{
		Use:   "publish ID --to DESTINATION[,DESTINATION]",
		Short: "Request publication of an asset; a publisher approves it before anything goes out",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("asset", args[0]); err != nil {
				return err
			}
			destinations := splitList(to)
			if len(destinations) == 0 {
				return usagef("--to is required")
			}
			resp, err := invoke[api.RunWorkflowResponse](cmd.Context(), a, apiclient.Call{
				Route: api.RoutePublicationsCreate,
				Body:  api.CreatePublicationRequest{AssetID: args[0], Destinations: destinations, Visibility: domain.Visibility(visibility)},
			})
			if err != nil {
				return err
			}
			return a.emitRun(resp)
		},
	}
	publish.Flags().StringSliceVar(&to, "to", nil, "destinations, comma separated")
	publish.Flags().StringVar(&visibility, "visibility", "", "private, unlisted or public")

	status := &cobra.Command{
		Use:   "status (ASSET-ID|PUB-nnn)",
		Short: "Show an asset's publications, or one publication",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			if err := validateRef("asset or publication", ref); err != nil {
				return err
			}
			ctx := cmd.Context()
			if isPublicationRef(ref) {
				pub, err := invoke[domain.Publication](ctx, a, apiclient.Call{Route: api.RoutePublicationsGet, Path: apiclient.PathParam("id", ref)})
				if err != nil {
					return err
				}
				return a.emit(pub, func(w io.Writer) { publicationTable(w, []domain.Publication{pub}) })
			}
			q := api.PublicationQuery{PageQuery: lf.page(), AssetID: ref, Destination: destFilter, Status: domain.PublicationStatus(pubStatus)}
			items, err := fetchList[domain.Publication](ctx, a, apiclient.Call{Route: api.RoutePublicationsList, Query: apiclient.Query(q)}, &lf)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				publicationTable(w, items.Items)
				a.noteNextPage(items.NextCursor)
			})
		},
	}
	lf.add(status)
	status.Flags().StringVar(&destFilter, "destination", "", "only this destination")
	status.Flags().StringVar(&pubStatus, "status", "", "only this publication state")

	sync := &cobra.Command{
		Use:   "sync (ASSET-ID|PUB-nnn)",
		Short: "Refresh publications from their destinations",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			if err := validateRef("asset or publication", ref); err != nil {
				return err
			}
			ctx := cmd.Context()
			targets := []string{ref}
			if !isPublicationRef(ref) {
				list, err := fetchList[domain.Publication](ctx, a, apiclient.Call{
					Route: api.RoutePublicationsList, Query: apiclient.Query(api.PublicationQuery{AssetID: ref}),
				}, &listFlags{all: true})
				if err != nil {
					return err
				}
				targets = targets[:0]
				for _, pub := range list.Items {
					targets = append(targets, string(pub.ID))
				}
			}
			out := api.List[domain.Publication]{Items: []domain.Publication{}}
			for _, target := range targets {
				pub, err := invoke[domain.Publication](ctx, a, apiclient.Call{
					Route: api.RoutePublicationsSync, Path: apiclient.PathParam("id", target), Body: struct{}{},
					IdempotencyKey: a.keyFor("sync", target),
				})
				if err != nil {
					return err
				}
				out.Items = append(out.Items, pub)
			}
			return a.emit(out, func(w io.Writer) { publicationTable(w, out.Items) })
		},
	}
	publish.AddCommand(status, sync)
	return publish
}

func (a *app) unpublishCommand() *cobra.Command {
	var (
		from []string
		all  bool
	)
	cmd := &cobra.Command{
		Use:   "unpublish (ASSET-ID --all | ASSET-ID --from DESTINATION[,DESTINATION] | PUB-nnn)",
		Short: "Withdraw publications at once, and from the public library (publisher or admin)",
		Long:  "Withdrawal is immediate and idempotent: repeating it is not an error and has no second effect.",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			if err := validateRef("asset or publication", ref); err != nil {
				return err
			}
			destinations := splitList(from)
			var (
				resp api.RunWorkflowResponse
				err  error
			)
			if isPublicationRef(ref) {
				if all || len(destinations) > 0 {
					return usagef("a publication ID takes neither --all nor --from")
				}
				resp, err = invoke[api.RunWorkflowResponse](cmd.Context(), a, apiclient.Call{Route: api.RoutePublicationsWithdraw, Path: apiclient.PathParam("id", ref)})
			} else {
				if all == (len(destinations) > 0) {
					return usagef("give exactly one of --all or --from")
				}
				resp, err = invoke[api.RunWorkflowResponse](cmd.Context(), a, apiclient.Call{
					Route: api.RoutePublicationsUnpub,
					Body:  api.WithdrawRequest{AssetID: ref, Destinations: destinations, All: all},
				})
			}
			if err != nil {
				return err
			}
			return a.emitRun(resp)
		},
	}
	cmd.Flags().StringSliceVar(&from, "from", nil, "destinations, comma separated")
	cmd.Flags().BoolVar(&all, "all", false, "every destination")
	return cmd
}
