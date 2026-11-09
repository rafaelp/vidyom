package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/version"
)

// Exit codes (I169).
const (
	exitOK          = 0
	exitFailed      = 1
	exitUsage       = 2
	exitInterrupted = 130
)

// Options are the seams of a run. The zero value means the real process.
type Options struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Keyring replaces the OS keyring.
	Keyring security.Keyring
	// ConfigDir replaces <config-dir> (I160).
	ConfigDir string
	Getenv    func(string) string
	Now       func() time.Time
	// StdinIsTerminal and StderrIsTerminal replace the terminal checks.
	StdinIsTerminal  func() bool
	StderrIsTerminal func() bool
	// ReadPassword reads a line from the terminal without echo. It must return, with
	// the terminal restored, when ctx ends.
	ReadPassword func(ctx context.Context) ([]byte, error)
	// Sleep replaces waits between retries.
	Sleep func(ctx context.Context, d time.Duration) error
	// SSHBinary replaces the ssh client program.
	SSHBinary string
	// Hostname replaces os.Hostname for the default device name.
	Hostname func() (string, error)
	// UserHomeDir replaces os.UserHomeDir for the default socket.
	UserHomeDir func() (string, error)
}

func (o Options) withDefaults() Options {
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.StdinIsTerminal == nil {
		stdin := o.Stdin
		o.StdinIsTerminal = func() bool { return isTerminal(stdin) }
	}
	if o.StderrIsTerminal == nil {
		stderr := o.Stderr
		o.StderrIsTerminal = func() bool { return isTerminal(stderr) }
	}
	if o.ReadPassword == nil {
		stdin := o.Stdin
		o.ReadPassword = func(ctx context.Context) ([]byte, error) { return readPassword(ctx, stdin) }
	}
	if o.Hostname == nil {
		o.Hostname = os.Hostname
	}
	if o.UserHomeDir == nil {
		o.UserHomeDir = os.UserHomeDir
	}
	return o
}

func isTerminal(v any) bool {
	file, ok := v.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// readPassword reads without echo. On interruption it restores the terminal itself,
// because the blocked read would otherwise leave echo off when the process exits.
func readPassword(ctx context.Context, stdin io.Reader) ([]byte, error) {
	file, ok := stdin.(*os.File)
	if !ok {
		return nil, errors.New("standard input is not a terminal")
	}
	fd := int(file.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	type result struct {
		value []byte
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := term.ReadPassword(fd)
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		_ = term.Restore(fd, state)
		return nil, ctx.Err()
	}
}

// Execute runs the CLI as the wiring surface declares it (master plan, section 9).
// args excludes the program name. It returns the process exit code.
func Execute(ctx context.Context, args []string) int {
	return Run(ctx, args, Options{})
}

// Run is Execute with explicit seams, for tests.
func Run(ctx context.Context, args []string, opts Options) int {
	a := &app{opts: opts.withDefaults()}
	defer a.closeClient()
	root := a.rootCommand()
	root.SetArgs(args)
	root.SetIn(a.opts.Stdin)
	root.SetOut(a.opts.Stdout)
	root.SetErr(a.opts.Stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		return a.report(ctx, err)
	}
	if a.exitCode != 0 {
		return a.exitCode
	}
	return exitOK
}

// app is one invocation of the CLI.
type app struct {
	opts Options

	jsonOutput     bool
	serverFlag     string
	tenantFlag     string
	tenantFlagSet  bool
	idempotencyKey string

	cfg      *clientConfig
	cfgDir   string
	kr       security.Keyring
	krOnce   sync.Once
	client   *apiclient.Client
	exitCode int
}

func (a *app) stdout() io.Writer { return a.opts.Stdout }
func (a *app) stderr() io.Writer { return a.opts.Stderr }

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "vidyom",
		Short:         "Client of the Vidyom daemon: uploads, jobs, approvals, the catalog and publications",
		Long:          "vidyom talks to a vidyomd daemon over a Unix socket on the same host or over HTTPS with device pairing.\nProcessing and server secrets stay on the server. Disconnecting never affects a running job.",
		Version:       version.Version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          usageArgs(cobra.NoArgs),
		RunE:          func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			a.tenantFlagSet = cmd.Flags().Changed("tenant")
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(flagError)

	flags := root.PersistentFlags()
	flags.BoolVar(&a.jsonOutput, "json", false, "print machine-readable JSON (NDJSON on streams); errors go to stderr as JSON")
	flags.StringVar(&a.serverFlag, "server", "", "server to use for this command (default: the current server)")
	flags.StringVar(&a.tenantFlag, "tenant", "", "tenant to use for this command (default: the server's current tenant)")
	flags.StringVar(&a.idempotencyKey, "idempotency-key", "", "fixed key for this command's mutating request, for scripts that retry")

	root.AddCommand(
		a.versionCommand(),
		a.daemonCommand(),
		a.serverCommand(),
		a.deviceCommand(),
		a.tenantCommand(),
		a.userCommand(),
		a.budgetCommand(),
		a.originalCommand(),
		a.assetCommand(),
		a.collectionCommand(),
		a.clipCommand(),
		a.approvalCommand(),
		a.workflowCommand(),
		a.nodeCommand(),
		a.jobCommand(),
		a.connectorCommand(),
		a.storageCommand(),
		a.publishingCommand(),
		a.publishCommand(),
		a.unpublishCommand(),
		a.secretCommand(),
		a.securityCommand(),
		a.logsCommand(),
		a.observabilityCommand(),
		a.doctorCommand(),
		a.pluginCommand(),
		a.hookCommand(),
		a.eventCommand(),
	)
	return root
}

// group is a command that only holds subcommands. A misspelled subcommand is a
// usage error rather than a help screen with exit code 0.
func group(use, short string, children ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(children...)
	return cmd
}

// usageError is a mistake in the command line (exit code 2).
type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

func usagef(format string, args ...any) error {
	return &usageError{message: fmt.Sprintf(format, args...)}
}

func flagError(_ *cobra.Command, err error) error { return &usageError{message: err.Error()} }

// usageArgs marks argument validation failures as usage errors.
func usageArgs(check cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := check(cmd, args); err != nil {
			return &usageError{message: err.Error()}
		}
		return nil
	}
}

func exactArgs(n int) cobra.PositionalArgs { return usageArgs(cobra.ExactArgs(n)) }
func noArgs() cobra.PositionalArgs         { return usageArgs(cobra.NoArgs) }

// report prints an error and returns the exit code.
func (a *app) report(ctx context.Context, err error) int {
	var usage *usageError
	switch {
	case errors.As(err, &usage), isCobraUsage(err):
		a.printError(errs.New(errs.CodeInvalidArgument, err.Error()), "")
		return exitUsage
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		a.printError(errs.New(errs.Code("interrupted"), "interrupted"), "")
		return exitInterrupted
	}
	var coded *errs.Error
	if !errors.As(err, &coded) {
		coded = errs.Wrap(errs.CodeInternal, err, "unexpected error")
	}
	a.printError(coded, a.hintFor(coded))
	return exitFailed
}

// isCobraUsage recognizes the errors cobra builds itself.
func isCobraUsage(err error) bool {
	message := err.Error()
	return strings.HasPrefix(message, "unknown command ") || strings.HasPrefix(message, "required flag(s) ")
}
