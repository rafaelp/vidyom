package cli

import (
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// ---- Assets ----

func assetRows(items []domain.Asset) [][]string {
	rows := make([][]string, 0, len(items))
	for _, asset := range items {
		rows = append(rows, []string{string(asset.ID), string(asset.Kind), asset.Title, string(asset.Status), string(asset.Visibility), asset.CurrentVersion})
	}
	return rows
}

var assetHeader = []string{"ID", "KIND", "TITLE", "STATUS", "VISIBILITY", "VERSION"}

func (a *app) listAssets(cmd *cobra.Command, q api.AssetQuery, lf *listFlags) error {
	q.PageQuery = lf.page()
	list, err := fetchList[domain.Asset](cmd.Context(), a, apiclient.Call{Route: api.RouteAssetsList, Query: apiclient.Query(q)}, lf)
	if err != nil {
		return err
	}
	return a.emit(list, func(w io.Writer) {
		table(w, assetHeader, assetRows(list.Items))
		a.noteNextPage(list.NextCursor)
	})
}

func (a *app) showAsset(cmd *cobra.Command, ref string) error {
	if err := validateRef("asset", ref); err != nil {
		return err
	}
	asset, err := invoke[domain.Asset](cmd.Context(), a, apiclient.Call{Route: api.RouteAssetsGet, Path: apiclient.PathParam("id", ref)})
	if err != nil {
		return err
	}
	return a.emit(asset, func(w io.Writer) { assetLines(w, asset) })
}

func mediaLine(file *domain.MediaFile) string {
	if file == nil {
		return ""
	}
	line := file.URI()
	if file.Profile != "" {
		line += " (" + file.Profile + ")"
	}
	if file.SizeBytes > 0 {
		line += " " + formatBytes(file.SizeBytes)
	}
	if file.Verified {
		line += " verified"
	}
	return line
}

func assetLines(w io.Writer, asset domain.Asset) {
	duration := ""
	if asset.DurationSeconds > 0 {
		duration = formatTimecode(asset.DurationSeconds)
	}
	transcript := ""
	if asset.Transcript != nil {
		transcript = fmt.Sprintf("%s, indexed: %s", asset.Transcript.Engine, yesNo(asset.Transcript.Indexed))
	}
	parent := ""
	if asset.Lineage != nil {
		parent = fmt.Sprintf("%s (root %s)", asset.Lineage.SourceAssetID, asset.Lineage.RootAssetID)
	}
	fields(w,
		kv("id", string(asset.ID)), kv("ulid", string(asset.ULID)), kv("kind", string(asset.Kind)),
		kv("title", asset.Title), kv("status", string(asset.Status)), kv("visibility", string(asset.Visibility)),
		kv("version", asset.CurrentVersion), kv("category", asset.Category), kv("collection", asset.Collection),
		kv("tags", strings.Join(asset.Tags, ", ")), kv("duration", duration), kv("language", asset.Language),
		kv("ai processing", string(asset.AIProcessing)),
		kv("master available", yesNo(asset.MasterAvailable)), kv("backup verified", yesNo(asset.BackupVerified)),
		kv("proxy available", yesNo(asset.ProxyAvailable)), kv("timeline validated", yesNo(asset.TimelineValidated)),
		kv("catalog sync", string(asset.CatalogSync)), kv("derived from", parent), kv("transcript", transcript),
		kv("source original", mediaLine(asset.SourceOriginal)), kv("working master", mediaLine(asset.WorkingMaster)),
		kv("proxy", mediaLine(asset.Proxy)), kv("backup", mediaLine(asset.Backup)),
		kv("created", formatTime(asset.CreatedAt)), kv("updated", formatTime(asset.UpdatedAt)),
	)
	if len(asset.Publications) > 0 {
		fmt.Fprintln(w)
		publicationTable(w, asset.Publications)
	}
}

func (a *app) assetCommand() *cobra.Command {
	var (
		lf             listFlags
		q              api.AssetQuery
		kind, status   string
		update         api.UpdateAssetRequest
		tags           []string
		title, descr   string
		category, coll string
		yes            bool
		transcriptVer  string
		sensitiveVer   string
		start, end     string
		reason         string
	)
	list := &cobra.Command{
		Use:   "list",
		Short: "List assets of any kind",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			q.Kind, q.Status = domain.AssetKind(kind), domain.AssetStatus(status)
			return a.listAssets(cmd, q, &lf)
		},
	}
	lf.add(list)
	list.Flags().StringVar(&kind, "kind", "", "original, clip or edited")
	list.Flags().StringVar(&status, "status", "", "lifecycle state")
	list.Flags().StringVar(&q.SourceID, "source", "", "derivatives of this asset")
	list.Flags().StringVar(&q.Collection, "collection", "", "collection name")

	show := &cobra.Command{
		Use:   "show ID",
		Short: "Show an asset",
		Args:  exactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.showAsset(cmd, args[0]) },
	}
	updateCmd := &cobra.Command{
		Use:   "update ID",
		Short: "Edit metadata; this never creates a media version",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("asset", args[0]); err != nil {
				return err
			}
			flags := cmd.Flags()
			if flags.Changed("title") {
				update.Title = &title
			}
			if flags.Changed("description") {
				update.Description = &descr
			}
			if flags.Changed("tags") {
				parsed := splitList(tags)
				if parsed == nil {
					parsed = []string{}
				}
				update.Tags = &parsed
			}
			if flags.Changed("category") {
				update.Category = &category
			}
			if flags.Changed("collection") {
				update.Collection = &coll
			}
			if update == (api.UpdateAssetRequest{}) {
				return usagef("nothing to change: give --title, --description, --tags, --category or --collection")
			}
			asset, err := invoke[domain.Asset](cmd.Context(), a, apiclient.Call{Route: api.RouteAssetsUpdate, Path: apiclient.PathParam("id", args[0]), Body: update})
			if err != nil {
				return err
			}
			return a.emit(asset, func(w io.Writer) { assetLines(w, asset) })
		},
	}
	updateCmd.Flags().StringVar(&title, "title", "", "title")
	updateCmd.Flags().StringVar(&descr, "description", "", "description")
	updateCmd.Flags().StringSliceVar(&tags, "tags", nil, "tags, comma separated")
	updateCmd.Flags().StringVar(&category, "category", "", "category")
	updateCmd.Flags().StringVar(&coll, "collection", "", "collection")

	lineage := &cobra.Command{
		Use:   "lineage ID",
		Short: "Show where an asset comes from and what derives from it",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("asset", args[0]); err != nil {
				return err
			}
			resp, err := invoke[api.LineageResponse](cmd.Context(), a, apiclient.Call{Route: api.RouteAssetsLineage, Path: apiclient.PathParam("id", args[0])})
			if err != nil {
				return err
			}
			return a.emit(resp, func(w io.Writer) {
				fields(w, kv("asset", string(resp.AssetID)))
				if resp.Lineage != nil {
					chain := make([]string, len(resp.Lineage.Chain))
					for i, c := range resp.Lineage.Chain {
						chain[i] = string(c)
					}
					fields(w, kv("method", string(resp.Lineage.Method)), kv("source", string(resp.Lineage.SourceAssetID)),
						kv("root", string(resp.Lineage.RootAssetID)), kv("chain", strings.Join(chain, " -> ")),
						kv("selection approval", string(resp.Lineage.SelectionApprovalID)))
					rows := make([][]string, 0, len(resp.Lineage.Segments))
					for _, s := range resp.Lineage.Segments {
						rows = append(rows, []string{string(s.SourceAssetID), s.SourceVersion, formatTimecode(s.StartSeconds), formatTimecode(s.EndSeconds)})
					}
					if len(rows) > 0 {
						table(w, []string{"SOURCE", "VERSION", "START", "END"}, rows)
					}
				}
				if len(resp.Children) > 0 {
					children := make([]string, len(resp.Children))
					for i, c := range resp.Children {
						children[i] = string(c)
					}
					fields(w, kv("derivatives", strings.Join(children, ", ")))
				}
			})
		},
	}
	versions := &cobra.Command{
		Use:   "versions ID",
		Short: "List an asset's media versions",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("asset", args[0]); err != nil {
				return err
			}
			items, err := fetchList[domain.AssetVersion](cmd.Context(), a, apiclient.Call{Route: api.RouteAssetsVersions, Path: apiclient.PathParam("id", args[0])}, nil)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, v := range items.Items {
					rows = append(rows, []string{v.Version, yesNo(v.Immutable), strconv.Itoa(len(v.Files)), formatTime(v.CreatedAt)})
				}
				table(w, []string{"VERSION", "IMMUTABLE", "FILES", "CREATED"}, rows)
			})
		},
	}
	deleteCmd := &cobra.Command{
		Use:   "delete ID",
		Short: "Delete an asset: needs asset.delete, starts a job, and keeps the fact in the audit",
		Long:  "Runs the delete-asset workflow. Publications must be withdrawn first. The archive copy is purged when the archive's minimum storage duration ends and stays inaccessible until then. The audit keeps the fact of the deletion without the content.",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("asset", args[0]); err != nil {
				return err
			}
			if err := a.confirm(fmt.Sprintf("Delete %s? This cannot be undone.", args[0]), yes); err != nil {
				return err
			}
			resp, err := invoke[api.RunWorkflowResponse](cmd.Context(), a, apiclient.Call{Route: api.RouteAssetsDelete, Path: apiclient.PathParam("id", args[0])})
			if err != nil {
				return err
			}
			return a.emitRun(resp)
		},
	}
	deleteCmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")

	setAI := &cobra.Command{
		Use:   "set-ai ID (local_only|external_allowed)",
		Short: "Set the asset's external AI policy (admin); records who and when",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("asset", args[0]); err != nil {
				return err
			}
			policy, err := parseAIPolicy(args[1])
			if err != nil {
				return err
			}
			asset, err := invoke[domain.Asset](cmd.Context(), a, apiclient.Call{
				Route: api.RouteAssetsAIProcessing, Path: apiclient.PathParam("id", args[0]),
				Body: api.SetAIProcessingRequest{AIProcessing: policy},
			})
			if err != nil {
				return err
			}
			return a.emit(asset, func(w io.Writer) {
				fmt.Fprintf(w, "%s: ai_processing is %s.\n", clean(string(asset.ID)), clean(string(asset.AIProcessing)))
			})
		},
	}
	consent := &cobra.Command{
		Use:   "consent ID",
		Short: "Show the asset's consent record for external AI processing",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("asset", args[0]); err != nil {
				return err
			}
			resp, err := invoke[api.ConsentResponse](cmd.Context(), a, apiclient.Call{Route: api.RouteAssetsConsent, Path: apiclient.PathParam("id", args[0])})
			if err != nil {
				return err
			}
			return a.emit(resp, func(w io.Writer) {
				fields(w, kv("ai processing", string(resp.AIProcessing)))
				if resp.Consent == nil {
					fmt.Fprintln(w, "No consent record: external processing was never authorized.")
					return
				}
				fields(w, kv("authorized by", resp.Consent.AuthorizedBy), kv("authorized at", formatTime(resp.Consent.AuthorizedAt)), kv("scope", resp.Consent.Scope))
			})
		},
	}
	transcript := &cobra.Command{
		Use:   "transcript ID",
		Short: "Print an asset's transcript with timestamps",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("asset", args[0]); err != nil {
				return err
			}
			resp, err := invoke[domain.Transcript](cmd.Context(), a, apiclient.Call{
				Route: api.RouteAssetsTranscript, Path: apiclient.PathParam("id", args[0]), Query: versionQuery(transcriptVer),
			})
			if err != nil {
				return err
			}
			return a.emit(resp, func(w io.Writer) {
				for _, s := range resp.Segments {
					speaker := ""
					if s.Speaker != "" {
						speaker = clean(s.Speaker) + ": "
					}
					fmt.Fprintf(w, "[%s] %s%s\n", formatTimecode(s.Start), speaker, clean(s.Text))
				}
			})
		},
	}
	transcript.Flags().StringVar(&transcriptVer, "version", "", "media version (default: current)")

	proxyLink := &cobra.Command{
		Use:   "proxy-link ID",
		Short: "Print a temporary link to the asset's proxy, for review",
		Args:  exactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.proxyLink(cmd, args[0]) },
	}
	export := &cobra.Command{
		Use:   "export ID",
		Short: "Export the asset's metadata package; starts a job",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("asset", args[0]); err != nil {
				return err
			}
			resp, err := invoke[api.RunWorkflowResponse](cmd.Context(), a, apiclient.Call{
				Route: api.RouteAssetsExport, Path: apiclient.PathParam("id", args[0]), Body: struct{}{},
			})
			if err != nil {
				return err
			}
			return a.emitRun(resp)
		},
	}

	sensitiveList := &cobra.Command{
		Use:   "list ID",
		Short: "List an asset's sensitive segments",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("asset", args[0]); err != nil {
				return err
			}
			items, err := fetchList[domain.SensitiveSegment](cmd.Context(), a, apiclient.Call{
				Route: api.RouteSensitiveList, Path: apiclient.PathParam("id", args[0]), Query: versionQuery(sensitiveVer),
			}, nil)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, s := range items.Items {
					rows = append(rows, []string{s.Version, formatTimecode(s.StartSeconds), formatTimecode(s.EndSeconds), s.Reason})
				}
				table(w, []string{"VERSION", "START", "END", "REASON"}, rows)
			})
		},
	}
	sensitiveList.Flags().StringVar(&sensitiveVer, "version", "", "media version (default: current)")

	segmentRequest := func() (api.SensitiveSegmentRequest, error) {
		first, err := parseTimecode(start)
		if err != nil {
			return api.SensitiveSegmentRequest{}, usagef("--start: %v", err)
		}
		last, err := parseTimecode(end)
		if err != nil {
			return api.SensitiveSegmentRequest{}, usagef("--end: %v", err)
		}
		if last <= first {
			return api.SensitiveSegmentRequest{}, usagef("--end must be after --start")
		}
		return api.SensitiveSegmentRequest{Version: sensitiveVer, StartSeconds: first, EndSeconds: last, Reason: reason}, nil
	}
	sensitiveMutation := func(use, short, route, verb string) *cobra.Command {
		c := &cobra.Command{
			Use:   use,
			Short: short,
			Args:  exactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := validateRef("asset", args[0]); err != nil {
					return err
				}
				req, err := segmentRequest()
				if err != nil {
					return err
				}
				if _, err := invoke[struct{}](cmd.Context(), a, apiclient.Call{Route: route, Path: apiclient.PathParam("id", args[0]), Body: req}); err != nil {
					return err
				}
				out := map[string]any{"asset": args[0], "start_seconds": req.StartSeconds, "end_seconds": req.EndSeconds, "result": verb}
				return a.emit(out, func(w io.Writer) {
					fmt.Fprintf(w, "%s %s to %s on %s.\n", strings.ToUpper(verb[:1])+verb[1:], formatTimecode(req.StartSeconds), formatTimecode(req.EndSeconds), clean(args[0]))
				})
			},
		}
		c.Flags().StringVar(&sensitiveVer, "version", "", "media version (default: current)")
		c.Flags().StringVar(&start, "start", "", "start, HH:MM:SS or seconds")
		c.Flags().StringVar(&end, "end", "", "end, HH:MM:SS or seconds")
		c.Flags().StringVar(&reason, "reason", "", "why; for example names, health or minors")
		return c
	}
	sensitive := group("sensitive", "Mark intervals excluded from public.db, public captions and external AI",
		sensitiveList,
		sensitiveMutation("add ID --start T --end T", "Mark an interval as sensitive", api.RouteSensitiveAdd, "marked"),
		sensitiveMutation("remove ID --start T --end T", "Unmark an interval", api.RouteSensitiveRemove, "unmarked"),
	)

	return group("asset", "Upload and manage assets",
		a.assetUploadCommand(), list, show, updateCmd, lineage, versions, deleteCmd, setAI, consent, transcript, proxyLink, export, sensitive)
}

func versionQuery(version string) url.Values {
	if version == "" {
		return nil
	}
	return url.Values{"version": {version}}
}

func parseAIPolicy(value string) (domain.AIProcessing, error) {
	switch domain.AIProcessing(value) {
	case domain.AILocalOnly, domain.AIExternalAllowed:
		return domain.AIProcessing(value), nil
	default:
		return "", usagef("invalid policy %q: expected local_only or external_allowed", value)
	}
}

func (a *app) proxyLink(cmd *cobra.Command, ref string) error {
	if err := validateRef("asset", ref); err != nil {
		return err
	}
	resp, err := invoke[api.ProxyLinkResponse](cmd.Context(), a, apiclient.Call{Route: api.RouteAssetsProxyLink, Path: apiclient.PathParam("id", ref)})
	if err != nil {
		return err
	}
	return a.emit(resp, func(w io.Writer) {
		fields(w, kv("url", resp.URL), kv("expires", formatTime(resp.ExpiresAt)))
	})
}

// emitRun reports a submitted job. The command returns at once: the job does not need
// this client (§5.5).
func (a *app) emitRun(resp api.RunWorkflowResponse) error {
	return a.emit(resp, func(w io.Writer) { runLines(w, resp) })
}

func runLines(w io.Writer, resp api.RunWorkflowResponse) {
	if resp.JobID == "" {
		fmt.Fprintf(w, "Accepted by %s %s; no job started.\n", clean(resp.Workflow), clean(resp.WorkflowVersion))
		return
	}
	fmt.Fprintf(w, "Started %s (%s %s), status %s.\n", clean(string(resp.JobID)), clean(resp.Workflow), clean(resp.WorkflowVersion), clean(string(resp.Status)))
	fmt.Fprintf(w, "Follow it with: vidyom job attach %s\n", clean(string(resp.JobID)))
}

// ---- Originals ----

func (a *app) originalCommand() *cobra.Command {
	var (
		lf     listFlags
		q      api.AssetQuery
		status string
		search api.SearchQuery
	)
	list := &cobra.Command{
		Use:   "list",
		Short: "List originals",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			q.Kind, q.Status = domain.KindOriginal, domain.AssetStatus(status)
			return a.listAssets(cmd, q, &lf)
		},
	}
	lf.add(list)
	list.Flags().StringVar(&status, "status", "", "lifecycle state")
	list.Flags().StringVar(&q.Collection, "collection", "", "collection name")

	show := &cobra.Command{
		Use:   "show ID",
		Short: "Show an original",
		Args:  exactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.showAsset(cmd, args[0]) },
	}
	searchCmd := &cobra.Command{
		Use:   "search QUERY",
		Short: "Search transcripts by words and by meaning",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			search.Q = args[0]
			resp, err := invoke[api.SearchResponse](cmd.Context(), a, apiclient.Call{Route: api.RouteSearch, Query: apiclient.Query(search)})
			if err != nil {
				return err
			}
			if resp.Items == nil {
				resp.Items = []domain.SearchHit{}
			}
			return a.emit(resp, func(w io.Writer) {
				rows := make([][]string, 0, len(resp.Items))
				for _, h := range resp.Items {
					rows = append(rows, []string{string(h.AssetID), h.Version, formatTimecode(h.StartSeconds), formatTimecode(h.EndSeconds), fmt.Sprintf("%.3f", h.Score), h.Source, excerpt(h.Text, 80)})
				}
				table(w, []string{"ASSET", "VERSION", "START", "END", "SCORE", "SOURCE", "TEXT"}, rows)
				a.note("mode: %s", resp.Mode)
			})
		},
	}
	searchCmd.Flags().StringVar(&search.AssetID, "asset", "", "search only this asset")
	searchCmd.Flags().StringVar(&search.Mode, "mode", "", "hybrid (default), lexical or semantic")
	searchCmd.Flags().IntVar(&search.Limit, "limit", 0, "maximum number of hits")

	return group("original", "Import, list and search originals", a.originalImportCommand(), list, show, searchCmd)
}

func excerpt(text string, limit int) string {
	text = clean(text)
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}

// ---- Collections ----

func (a *app) collectionCommand() *cobra.Command {
	var name string
	list := &cobra.Command{
		Use:   "list",
		Short: "List collections and their external AI policy",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := fetchList[domain.Collection](cmd.Context(), a, apiclient.Call{Route: api.RouteCollectionsList}, nil)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, c := range items.Items {
					authorized := ""
					if c.AIConsent != nil {
						authorized = c.AIConsent.AuthorizedBy
					}
					rows = append(rows, []string{c.Name, string(c.AIProcessing), authorized})
				}
				table(w, []string{"NAME", "AI PROCESSING", "AUTHORIZED BY"}, rows)
			})
		},
	}
	setAI := &cobra.Command{
		Use:   "set-ai --name NAME (local_only|external_allowed)",
		Short: "Set a collection's external AI policy (admin); records who and when",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return usagef("--name is required")
			}
			policy, err := parseAIPolicy(args[0])
			if err != nil {
				return err
			}
			resp, err := invoke[domain.Collection](cmd.Context(), a, apiclient.Call{
				Route: api.RouteCollectionsAIProcessing, Path: apiclient.PathParam("name", name),
				Body: api.SetAIProcessingRequest{AIProcessing: policy},
			})
			if err != nil {
				return err
			}
			return a.emit(resp, func(w io.Writer) {
				fmt.Fprintf(w, "Collection %s: ai_processing is %s.\n", clean(resp.Name), clean(string(resp.AIProcessing)))
			})
		},
	}
	setAI.Flags().StringVar(&name, "name", "", "collection name")
	return group("collection", "Manage collections", list, setAI)
}

// ---- Clips ----

func (a *app) clipCommand() *cobra.Command {
	var (
		req          api.CreateClipRequest
		workflow     string
		mode         string
		destinations []string
		lf           listFlags
		source       string
		status       string
	)
	create := &cobra.Command{
		Use:   "create --source ID --prompt TEXT",
		Short: "Request a semantic clip; it stops at the selection approval",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if req.SourceID == "" || req.Prompt == "" {
				return usagef("--source and --prompt are required")
			}
			if err := validateRef("source", req.SourceID); err != nil {
				return err
			}
			req.Mode = api.ClipMode(mode)
			req.Destinations = splitList(destinations)
			if workflow == "" || workflow == "create-clip" {
				resp, err := invoke[api.RunWorkflowResponse](cmd.Context(), a, apiclient.Call{Route: api.RouteClipsCreate, Body: req})
				if err != nil {
					return err
				}
				return a.emitRun(resp)
			}
			// A tenant's own clip workflow takes the same inputs.
			inputs := map[string]any{"source_id": req.SourceID, "prompt": req.Prompt}
			if req.Mode != "" {
				inputs["mode"] = string(req.Mode)
			}
			if req.TargetDurationSeconds > 0 {
				inputs["target_duration_seconds"] = req.TargetDurationSeconds
			}
			if len(req.Destinations) > 0 {
				inputs["destinations"] = req.Destinations
			}
			return a.runRegisteredWorkflow(cmd, workflow, req.WorkflowVersion, inputs)
		},
	}
	create.Flags().StringVar(&req.SourceID, "source", "", "the indexed asset to clip from")
	create.Flags().StringVar(&req.Prompt, "prompt", "", "what to find, in natural language")
	create.Flags().StringVar(&workflow, "workflow", "", "workflow to run (default: create-clip)")
	create.Flags().StringVar(&req.WorkflowVersion, "workflow-version", "", "workflow version (default: latest)")
	create.Flags().StringVar(&mode, "mode", "", "complete_story, short or best_moments")
	create.Flags().Float64Var(&req.TargetDurationSeconds, "target-duration", 0, "target duration in seconds, for short")
	create.Flags().StringSliceVar(&destinations, "destination", nil, "publishing destinations, comma separated")

	preview := &cobra.Command{
		Use:   "preview CLIP-nnn",
		Short: "Print a temporary link to the clip's proxy, for review",
		Args:  exactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.proxyLink(cmd, args[0]) },
	}
	approve := &cobra.Command{
		Use:   "approve CLIP-nnn",
		Short: "Shortcut: approve the clip's single pending publication approval",
		Long:  "Shortcut for 'approval approve' on the clip's pending publication approval. The selection gate happens before the clip exists, so it is decided with 'approval approve APR-nnn'.",
		Args:  exactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.approveClip(cmd, args[0]) },
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List clips",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.listAssets(cmd, api.AssetQuery{Kind: domain.KindClip, SourceID: source, Status: domain.AssetStatus(status)}, &lf)
		},
	}
	lf.add(list)
	list.Flags().StringVar(&source, "source", "", "clips of this asset")
	list.Flags().StringVar(&status, "status", "", "lifecycle state")

	return group("clip", "Create, review and list clips", create, preview, approve, list)
}

// ---- Approvals ----

// tenantResponsibility is the statement of D29.
const tenantResponsibility = "Image and voice authorizations from speakers and participants are the tenant's responsibility. Vidyom does not track or enforce them."

// responsibilityNotices is what an approval screen must state for this kind (I176).
func responsibilityNotices(kind domain.ApprovalKind) []string {
	switch kind {
	case domain.ApprovalPublication, domain.ApprovalVisibilityChange:
		return []string{tenantResponsibility}
	default:
		return nil
	}
}

type approvalView struct {
	domain.Approval
	Notices []string `json:"notices,omitempty"`
}

func viewApproval(approval domain.Approval) approvalView {
	return approvalView{Approval: approval, Notices: responsibilityNotices(approval.Kind)}
}

func approvalLines(w io.Writer, view approvalView) {
	asset := string(view.AssetID)
	if view.AssetVersion != "" {
		asset += " (" + view.AssetVersion + ")"
	}
	fields(w,
		kv("id", string(view.ID)), kv("kind", string(view.Kind)), kv("status", string(view.Status)),
		kv("asset", asset), kv("job", string(view.JobID)), kv("node", view.NodeID),
		kv("approvers", strings.Join(view.Approvers, ", ")), kv("preview", view.Preview),
		kv("adjustable", yesNo(view.AllowAdjust)), kv("expires", formatTimePtr(view.ExpiresAt)),
		kv("decided by", view.DecidedBy), kv("decided at", formatTimePtr(view.DecidedAt)), kv("reason", view.Reason),
	)
	if len(view.Segments) > 0 {
		rows := make([][]string, 0, len(view.Segments))
		for _, s := range view.Segments {
			rows = append(rows, []string{string(s.SourceAssetID), s.SourceVersion, formatTimecode(s.StartSeconds), formatTimecode(s.EndSeconds)})
		}
		table(w, []string{"SOURCE", "VERSION", "START", "END"}, rows)
	}
	if len(view.Evidence) > 0 {
		fmt.Fprintf(w, "evidence:\n%s\n", prettyJSON(view.Evidence))
	}
	for _, notice := range view.Notices {
		fmt.Fprintf(w, "\n%s\n", notice)
	}
}

type approveFlags struct {
	start, end string
	segments   []string
	note       string
}

func (f *approveFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.start, "start", "", "adjusted start, HH:MM:SS or seconds")
	cmd.Flags().StringVar(&f.end, "end", "", "adjusted end, HH:MM:SS or seconds")
	cmd.Flags().StringArrayVar(&f.segments, "segment", nil, "adjusted interval START-END; repeat for several")
	cmd.Flags().StringVar(&f.note, "note", "", "note recorded with the decision")
}

// request builds the decision. Asking to adjust an approval that does not allow it is
// refused, because the server would ignore it and the reviewer would have approved
// something other than what they asked (I176).
func (f *approveFlags) request(approval domain.Approval) (api.ApproveRequest, error) {
	req := api.ApproveRequest{Note: f.note}
	hasInterval := f.start != "" || f.end != ""
	if !hasInterval && len(f.segments) == 0 {
		return req, nil
	}
	if !approval.AllowAdjust {
		return req, usagef("%s does not allow adjusting the interval", approval.ID)
	}
	if hasInterval && len(f.segments) > 0 {
		return req, usagef("give --start and --end, or --segment, not both")
	}
	if hasInterval {
		if f.start == "" || f.end == "" {
			return req, usagef("--start and --end go together")
		}
		first, err := parseTimecode(f.start)
		if err != nil {
			return req, usagef("--start: %v", err)
		}
		last, err := parseTimecode(f.end)
		if err != nil {
			return req, usagef("--end: %v", err)
		}
		if last <= first {
			return req, usagef("--end must be after --start")
		}
		req.StartSeconds, req.EndSeconds = &first, &last
		return req, nil
	}
	for _, raw := range f.segments {
		from, to, ok := strings.Cut(raw, "-")
		if !ok {
			return req, usagef("invalid --segment %q: expected START-END", raw)
		}
		first, err := parseTimecode(from)
		if err != nil {
			return req, usagef("--segment: %v", err)
		}
		last, err := parseTimecode(to)
		if err != nil {
			return req, usagef("--segment: %v", err)
		}
		if last <= first {
			return req, usagef("invalid --segment %q: the end must be after the start", raw)
		}
		req.Segments = append(req.Segments, domain.Segment{
			SourceAssetID: approval.AssetID, SourceVersion: approval.AssetVersion,
			StartSeconds: first, EndSeconds: last,
		})
	}
	return req, nil
}

func (a *app) decideApproval(cmd *cobra.Command, approval domain.Approval, flags approveFlags) error {
	req, err := flags.request(approval)
	if err != nil {
		return err
	}
	for _, notice := range responsibilityNotices(approval.Kind) {
		a.note("%s", notice)
	}
	decided, err := invoke[domain.Approval](cmd.Context(), a, apiclient.Call{
		Route: api.RouteApprovalsApprove, Path: apiclient.PathParam("id", string(approval.ID)), Body: req,
	})
	if err != nil {
		return err
	}
	view := viewApproval(decided)
	return a.emit(view, func(w io.Writer) {
		fmt.Fprintf(w, "Approved %s (%s). The waiting job resumes on the server.\n", clean(string(decided.ID)), clean(string(decided.Kind)))
	})
}

// approveClip is the shortcut of §11.2. It never guesses among several (I176).
func (a *app) approveClip(cmd *cobra.Command, ref string) error {
	if err := validateRef("clip", ref); err != nil {
		return err
	}
	list, err := fetchList[domain.Approval](cmd.Context(), a, apiclient.Call{
		Route: api.RouteApprovalsList,
		Query: apiclient.Query(api.ApprovalQuery{AssetID: ref, Kind: domain.ApprovalPublication, Status: domain.ApprovalPending}),
	}, nil)
	if err != nil {
		return err
	}
	switch len(list.Items) {
	case 0:
		return errs.New(errs.CodeFailedPrecondition, fmt.Sprintf("%s has no pending publication approval; a selection gate is decided with 'vidyom approval approve APR-nnn'", ref))
	case 1:
		return a.decideApproval(cmd, list.Items[0], approveFlags{})
	default:
		ids := make([]string, len(list.Items))
		for i, item := range list.Items {
			ids[i] = string(item.ID)
		}
		return errs.New(errs.CodeFailedPrecondition, fmt.Sprintf("%s has %d pending publication approvals (%s); decide one with 'vidyom approval approve APR-nnn'", ref, len(ids), strings.Join(ids, ", ")))
	}
}

func (a *app) approvalCommand() *cobra.Command {
	var (
		lf           listFlags
		status, kind string
		assetID      string
		flags        approveFlags
		reason       string
	)
	list := &cobra.Command{
		Use:   "list",
		Short: "List approvals",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := api.ApprovalQuery{PageQuery: lf.page(), Status: domain.ApprovalStatus(status), Kind: domain.ApprovalKind(kind), AssetID: assetID}
			items, err := fetchList[domain.Approval](cmd.Context(), a, apiclient.Call{Route: api.RouteApprovalsList, Query: apiclient.Query(q)}, &lf)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, ap := range items.Items {
					rows = append(rows, []string{string(ap.ID), string(ap.Kind), string(ap.Status), string(ap.AssetID), string(ap.JobID), formatTime(ap.CreatedAt)})
				}
				table(w, []string{"ID", "KIND", "STATUS", "ASSET", "JOB", "CREATED"}, rows)
				a.noteNextPage(items.NextCursor)
			})
		},
	}
	lf.add(list)
	list.Flags().StringVar(&status, "status", "", "pending, approved, rejected or expired")
	list.Flags().StringVar(&kind, "kind", "", "clip_selection, publication, visibility_change, metadata_change or other")
	list.Flags().StringVar(&assetID, "asset", "", "approvals of this asset")

	show := &cobra.Command{
		Use:   "show APR-nnn",
		Short: "Show an approval with its evidence",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("approval", args[0]); err != nil {
				return err
			}
			approval, err := invoke[domain.Approval](cmd.Context(), a, apiclient.Call{Route: api.RouteApprovalsGet, Path: apiclient.PathParam("id", args[0])})
			if err != nil {
				return err
			}
			view := viewApproval(approval)
			return a.emit(view, func(w io.Writer) { approvalLines(w, view) })
		},
	}
	approve := &cobra.Command{
		Use:   "approve APR-nnn [--start T --end T]",
		Short: "Approve a gate, optionally adjusting the interval",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("approval", args[0]); err != nil {
				return err
			}
			approval, err := invoke[domain.Approval](cmd.Context(), a, apiclient.Call{Route: api.RouteApprovalsGet, Path: apiclient.PathParam("id", args[0])})
			if err != nil {
				return err
			}
			return a.decideApproval(cmd, approval, flags)
		},
	}
	flags.add(approve)
	reject := &cobra.Command{
		Use:   "reject APR-nnn --reason TEXT",
		Short: "Reject a gate",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRef("approval", args[0]); err != nil {
				return err
			}
			if reason == "" {
				return usagef("--reason is required")
			}
			decided, err := invoke[domain.Approval](cmd.Context(), a, apiclient.Call{
				Route: api.RouteApprovalsReject, Path: apiclient.PathParam("id", args[0]), Body: api.RejectRequest{Reason: reason},
			})
			if err != nil {
				return err
			}
			return a.emit(viewApproval(decided), func(w io.Writer) { fmt.Fprintf(w, "Rejected %s.\n", clean(string(decided.ID))) })
		},
	}
	reject.Flags().StringVar(&reason, "reason", "", "why; recorded with the decision")
	return group("approval", "List and decide approvals", list, show, approve, reject)
}
