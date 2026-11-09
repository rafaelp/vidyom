package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

const defaultIngestWorkflow = "ingest-original"

// ingestFlags are the flags of `original import` and `asset upload`.
type ingestFlags struct {
	workflow string
	version  string
	title    string
	category string
	speaker  string
	inputs   []string
	chunk    int64
}

func (f *ingestFlags) add(cmd *cobra.Command, workflowHelp string) {
	cmd.Flags().StringVar(&f.workflow, "workflow", "", workflowHelp)
	cmd.Flags().StringVar(&f.version, "workflow-version", "", "workflow version (default: latest)")
	cmd.Flags().StringVar(&f.title, "title", "", "title (default: the file's name)")
	cmd.Flags().StringVar(&f.category, "type", "", "category of the original, such as yoga_class")
	cmd.Flags().StringVar(&f.speaker, "speaker", "", "speaker or instructor")
	cmd.Flags().StringArrayVar(&f.inputs, "input", nil, "other workflow input, key=value; repeatable")
	cmd.Flags().Int64Var(&f.chunk, "chunk-bytes", 0, "requested chunk size; the server may adjust it")
}

// ingestResult is the answer of a command that may upload and may start a job.
type ingestResult struct {
	Upload *domain.Upload           `json:"upload,omitempty"`
	Job    *api.RunWorkflowResponse `json:"job,omitempty"`
}

func (a *app) originalImportCommand() *cobra.Command {
	var (
		flags      ingestFlags
		serverPath bool
	)
	cmd := &cobra.Command{
		Use:   "import PATH",
		Short: "Upload a file from this machine and ingest it; --server-path names a file on the server",
		Long: "PATH is a file on this machine: it is uploaded with checkpoints and then ingested. Interrupted, the same command resumes.\n" +
			"With --server-path, PATH is a file on the server, inside the daemon's ingestRoots, and nothing is uploaded.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.workflow == "" {
				flags.workflow = defaultIngestWorkflow
			}
			if serverPath {
				return a.ingestServerFile(cmd, args[0], flags)
			}
			return a.ingestClientFile(cmd, args[0], flags)
		},
	}
	flags.add(cmd, "workflow to run (default: "+defaultIngestWorkflow+")")
	cmd.Flags().BoolVar(&serverPath, "server-path", false, "PATH is a file on the server, not on this machine")
	return cmd
}

func (a *app) assetUploadCommand() *cobra.Command {
	var flags ingestFlags
	cmd := &cobra.Command{
		Use:   "upload PATH",
		Short: "Upload a file from this machine, with checkpoints; --workflow also starts ingestion",
		Long:  "Uploads a client file. Interrupted, the same command resumes. Without --workflow it prints the reference to use later, such as upload:UPL-001.",
		Args:  exactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.ingestClientFile(cmd, args[0], flags) },
	}
	flags.add(cmd, "workflow to start once the upload is verified")
	return cmd
}

// fileInputName picks the workflow's file input: the only declared one, otherwise
// "file" as §6.3 names it.
func fileInputName(specs map[string]wfdef.InputSpec) string {
	found := ""
	for name, spec := range specs {
		if spec.Type != wfdef.InputFile {
			continue
		}
		if found != "" {
			return "file"
		}
		found = name
	}
	if found == "" {
		return "file"
	}
	return found
}

// ingestInputs merges the flags over what the workflow declares (I173). A default
// title is sent only when the workflow has one; an explicit flag always is, so the
// server's validation can refuse it.
func ingestInputs(flags ingestFlags, specs map[string]wfdef.InputSpec, baseName string) (map[string]any, error) {
	inputs, err := parseInputs(flags.inputs, specs)
	if err != nil {
		return nil, err
	}
	if flags.title != "" {
		inputs["title"] = flags.title
	} else if _, declared := specs["title"]; declared {
		if _, given := inputs["title"]; !given {
			inputs["title"] = strings.TrimSuffix(baseName, filepath.Ext(baseName))
		}
	}
	if flags.category != "" {
		inputs["category"] = flags.category
	}
	if flags.speaker != "" {
		inputs["speaker"] = flags.speaker
	}
	return inputs, nil
}

func (a *app) workflowSpecs(ctx context.Context, workflow, version string) (map[string]wfdef.InputSpec, error) {
	detail, err := invoke[api.WorkflowDetail](ctx, a, apiclient.Call{
		Route: api.RouteWorkflowsGet, Path: apiclient.PathParam("id", workflow), Query: versionQuery(version),
	})
	if err != nil {
		return nil, err
	}
	return detail.Inputs, nil
}

func (a *app) ingestServerFile(cmd *cobra.Command, path string, flags ingestFlags) error {
	uri, err := parseFileReference("server:" + path)
	if err != nil {
		return usagef("%v", err)
	}
	ctx := cmd.Context()
	specs, err := a.workflowSpecs(ctx, flags.workflow, flags.version)
	if err != nil {
		return err
	}
	inputs, err := ingestInputs(flags, specs, filepath.Base(path))
	if err != nil {
		return err
	}
	inputs[fileInputName(specs)] = uri
	return a.runRegisteredWorkflow(cmd, flags.workflow, flags.version, inputs)
}

func (a *app) ingestClientFile(cmd *cobra.Command, path string, flags ingestFlags) error {
	ctx := cmd.Context()
	abs, err := filepath.Abs(path)
	if err != nil {
		return errs.Wrap(errs.CodeInvalidArgument, err, "cannot resolve the file path")
	}
	if _, err := os.Stat(abs); err != nil {
		return errs.Wrap(errs.CodeInvalidArgument, err, "cannot read the file to upload")
	}

	// Before any byte moves: a 10 GB transfer must not start against a workflow that
	// does not exist or inputs that cannot be sent (I173).
	var (
		specs  map[string]wfdef.InputSpec
		inputs map[string]any
	)
	if flags.workflow != "" {
		if specs, err = a.workflowSpecs(ctx, flags.workflow, flags.version); err != nil {
			return err
		}
		if inputs, err = ingestInputs(flags, specs, filepath.Base(abs)); err != nil {
			return err
		}
	} else if flags.title != "" || flags.category != "" || flags.speaker != "" || len(flags.inputs) > 0 {
		return usagef("--title, --type, --speaker and --input need --workflow")
	}

	client, err := a.api()
	if err != nil {
		return err
	}
	serverName, _, _, err := a.resolveServer()
	if err != nil {
		return err
	}
	store := apiclient.FileCheckpoints{Dir: filepath.Join(a.cfgDir, "uploads")}
	key := strings.Join([]string{serverName, client.Tenant(), abs}, "\x00")

	progress := a.uploadProgress()
	result, err := client.Upload(ctx, apiclient.UploadOptions{
		Path: abs, CheckpointKey: key, Store: store, ChunkBytes: flags.chunk,
		OnProgress: progress.update,
		OnRetry: func(cause error, delay time.Duration) {
			progress.pause()
			a.note("upload interrupted (%s), retrying in %s", errs.Public(cause).Message, delay.Round(100*time.Millisecond))
		},
	})
	progress.done()
	if err != nil {
		if errors.Is(err, context.Canceled) || apiclient.IsTransient(err) {
			a.note("The upload can be resumed: run the same command again.")
		}
		return err
	}
	if result.Resumed {
		a.note("Resumed the upload from %s.", formatBytes(result.ResumedFrom))
	}

	out := ingestResult{Upload: &result.Upload}
	if flags.workflow == "" {
		_ = store.Delete(key)
		return a.emit(out, func(w io.Writer) {
			fmt.Fprintf(w, "Uploaded %s: %s, SHA-256 verified.\n", clean(result.Upload.Filename), formatBytes(result.Upload.SizeBytes))
			fmt.Fprintf(w, "Reference it as: upload:%s\n", clean(string(result.Upload.ID)))
		})
	}

	inputs[fileInputName(specs)] = "upload:" + string(result.Upload.ID)
	encoded, err := json.Marshal(inputs)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot encode the workflow inputs")
	}
	// The same upload and the same inputs always mean the same job, so a kill between
	// complete and this call cannot start two (I170).
	runKey := a.idempotencyKey
	if runKey == "" {
		runKey = apiclient.DerivedKey("run", string(result.Upload.ULID), flags.workflow, flags.version, canonicalJSON(inputs))
	}
	job, err := apiclient.Invoke[api.RunWorkflowResponse](ctx, client, apiclient.Call{
		Route: api.RouteWorkflowsRun, Path: apiclient.PathParam("id", flags.workflow),
		Body:           api.RunWorkflowRequest{WorkflowVersion: flags.version, Inputs: encoded},
		IdempotencyKey: runKey,
	})
	if err != nil {
		return err
	}
	_ = store.Delete(key)
	out.Job = &job
	return a.emit(out, func(w io.Writer) {
		fmt.Fprintf(w, "Uploaded %s as %s, SHA-256 verified.\n", clean(result.Upload.Filename), clean(string(result.Upload.ID)))
		runLines(w, job)
	})
}

// canonicalJSON renders a value with sorted keys. encoding/json already sorts maps,
// so this only hides the error path of values built by this package.
func canonicalJSON(value map[string]any) string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		raw, _ := json.Marshal(value[key])
		b.WriteString(key)
		b.WriteByte('=')
		b.Write(raw)
		b.WriteByte(0)
	}
	return b.String()
}

// progressLine draws the upload's progress on a terminal and stays quiet elsewhere.
type progressLine struct {
	a        *app
	mu       sync.Mutex
	last     time.Time
	drawn    bool
	disabled bool
}

func (a *app) uploadProgress() *progressLine {
	return &progressLine{a: a, disabled: a.jsonOutput || !a.opts.StderrIsTerminal()}
}

func (p *progressLine) update(phase apiclient.UploadPhase, done, total int64) {
	if p.disabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.a.opts.Now()
	if done < total && now.Sub(p.last) < 200*time.Millisecond {
		return
	}
	p.last = now
	percent := 100.0
	if total > 0 {
		percent = float64(done) / float64(total) * 100
	}
	fmt.Fprintf(p.a.stderr(), "\r%s %s of %s (%.1f%%)   ", phase, formatBytes(done), formatBytes(total), percent)
	p.drawn = true
}

// pause ends the line so a message can follow.
func (p *progressLine) pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.drawn {
		fmt.Fprintln(p.a.stderr())
		p.drawn = false
	}
}

func (p *progressLine) done() { p.pause() }
