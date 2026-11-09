// Command vidyomd is the Vidyom daemon: API, authentication, scheduler, workflow
// engine and state (§5.1). `vidyomd init` initializes a data directory, and
// `vidyomd start` runs the daemon.
//
// Components are assembled in wire.go, which belongs to the integration stage and
// sets wireDeps. Without it, the commands that need components exit with a clear
// error and open no listener.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/version"
	"github.com/rafaelp/vidyom/internal/daemon"
)

// wireDeps assembles the daemon's components (master plan, section 9). The returned
// function releases them. cfg.Bootstrapping is true when called for `vidyomd init`.
var wireDeps func(ctx context.Context, cfg config.Daemon) (daemon.Deps, func(), error)

const usage = `vidyomd is the Vidyom daemon.

Usage:
  vidyomd init --tenant T --admin E [--config-dir D] [--worker-token-file F] [--json]
  vidyomd init --generate-master-key P
  vidyomd start [--config-dir D] [--detach]
  vidyomd version

The configuration directory holds daemon.json. It comes from --config-dir, then
` + daemon.ConfigDirEnv + `; there is no implicit location.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "init":
		err = runInit(args[1:], stdout, stderr)
	case "start":
		err = runStart(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintf(stdout, "vidyomd %s\n", version.Version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "vidyomd: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		fmt.Fprintf(stderr, "vidyomd: %v\n", err)
		return 1
	}
}

var errUsage = errors.New("usage")

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("vidyomd "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		return errUsage
	}
	return nil
}

func notWired() error {
	return errors.New("not wired: this build has no cmd/vidyomd/wire.go, so no component is assembled and no listener opens")
}

// signalContext ends on SIGINT or SIGTERM. SIGHUP is the daemon's own (I34).
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runInit(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("init", stderr)
	configDir := fs.String("config-dir", "", "directory holding daemon.json")
	tenant := fs.String("tenant", "", "the first tenant's slug")
	admin := fs.String("admin", "", "the first admin's email")
	tokenFile := fs.String("worker-token-file", "", "where to store the worker token (default <data-dir>/secrets/worker.token)")
	masterKey := fs.String("generate-master-key", "", "write a new master key to this path, mode 0400 (I10)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	if *masterKey != "" {
		if err := daemon.GenerateMasterKey(*masterKey); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "wrote a new master key to %s; it is plaintext on this host, keep it as a mounted secret (I10)\n", *masterKey)
		if *tenant == "" && *admin == "" {
			return nil
		}
	}

	if *tenant == "" || *admin == "" {
		fmt.Fprintln(stderr, "vidyomd init needs --tenant and --admin")
		return errUsage
	}
	dir, err := daemon.ResolveConfigDir(*configDir)
	if err != nil {
		return err
	}
	cfg, err := daemon.LoadConfig(dir)
	if err != nil {
		return err
	}
	if wireDeps == nil {
		return notWired()
	}
	cfg.Bootstrapping = true

	ctx, cancel := signalContext()
	defer cancel()
	deps, cleanup, err := wireDeps(ctx, cfg)
	if err != nil {
		return fmt.Errorf("assemble the daemon: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	result, err := daemon.Init(ctx, cfg, deps.Bootstrap, daemon.InitOptions{
		Tenant:          domain.TenantID(*tenant),
		AdminEmail:      *admin,
		WorkerTokenFile: *tokenFile,
	})
	if err != nil {
		return err
	}
	return printInit(stdout, result, *asJSON)
}

func printInit(stdout io.Writer, r daemon.InitResult, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	fmt.Fprintf(stdout, "Initialized tenant %s with first admin %s.\n", r.Tenant, r.Admin)
	fmt.Fprintf(stdout, "Pairing code, shown once: %s\n", r.PairingCode)
	if !r.PairingExpiresAt.IsZero() {
		fmt.Fprintf(stdout, "It expires at %s.\n", r.PairingExpiresAt.UTC().Format("2006-01-02 15:04:05 MST"))
	}
	if r.TLSFingerprint != "" {
		fmt.Fprintf(stdout, "TLS fingerprint to pin: %s\n", r.TLSFingerprint)
	}
	fmt.Fprintf(stdout, "Worker token stored in %s.\n", r.WorkerTokenFile)
	return nil
}

func runStart(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("start", stderr)
	configDir := fs.String("config-dir", "", "directory holding daemon.json")
	detach := fs.Bool("detach", false, "run in the background; returns once the daemon is listening")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	dir, err := daemon.ResolveConfigDir(*configDir)
	if err != nil {
		return err
	}
	cfg, err := daemon.LoadConfig(dir)
	if err != nil {
		return err
	}
	// Refuse here too, so that an unconfigured master key is reported without
	// assembling anything (D13).
	if err := daemon.ValidateConfig(cfg); err != nil {
		return err
	}
	if wireDeps == nil {
		return notWired()
	}
	if *detach {
		return startDetached(cfg, dir, stdout)
	}

	ctx, cancel := signalContext()
	defer cancel()
	notifyReady, err := readyNotifier()
	if err != nil {
		return err
	}
	deps, cleanup, err := wireDeps(ctx, cfg)
	if err != nil {
		return fmt.Errorf("assemble the daemon: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if deps.Logger == nil {
		deps.Logger = newLogger(stderr)
	}
	previous := deps.OnReady
	deps.OnReady = func(infos []daemon.ListenerInfo) {
		if previous != nil {
			previous(infos)
		}
		notifyReady()
	}
	return daemon.Run(ctx, cfg, deps)
}

// newLogger is the default when wiring supplies none: one JSON object per line with
// the field names of §10.1.
func newLogger(w io.Writer) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.TimeKey:
				a.Key = "timestamp"
			case slog.MessageKey:
				a.Key = "event"
			}
			return a
		},
	})
	return slog.New(handler).With("service", "vidyomd")
}
