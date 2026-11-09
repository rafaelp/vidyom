package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/rafaelp/vidyom/internal/contract/config"
)

// ConfigFileName is the daemon's settings file inside the configuration directory
// (I34).
const ConfigFileName = "daemon.json"

// ConfigDirEnv names the configuration directory when --config-dir is not given.
// VIDYOM_CONFIG_DIR already means the client's directory (I160).
const ConfigDirEnv = "VIDYOMD_CONFIG_DIR"

// ResolveConfigDir applies --config-dir, then the environment. There is no implicit
// location, so a daemon can never start from a file nobody chose (I138).
func ResolveConfigDir(flagValue string) (string, error) {
	dir := flagValue
	if dir == "" {
		dir = os.Getenv(ConfigDirEnv)
	}
	if dir == "" {
		return "", fmt.Errorf("no configuration directory: pass --config-dir or set %s", ConfigDirEnv)
	}
	return filepath.Abs(dir)
}

// LoadConfig reads <dir>/daemon.json strictly: a misspelled key fails closed rather
// than silently leaving a security setting at its zero value. Defaults are applied.
// It does not validate; ValidateConfig does, so `init` can check less than `start`.
func LoadConfig(dir string) (config.Daemon, error) {
	path := filepath.Join(dir, ConfigFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return config.Daemon{}, fmt.Errorf("read daemon configuration: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg config.Daemon
	if err := dec.Decode(&cfg); err != nil {
		return config.Daemon{}, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return config.Daemon{}, fmt.Errorf("%s: unexpected data after the configuration", path)
	}
	cfg.ConfigDir = dir
	if err := ApplyDefaults(&cfg); err != nil {
		return config.Daemon{}, err
	}
	return cfg, nil
}

// ApplyDefaults fills what daemon.json may leave out. Directories default under
// dataDir. With no listener at all, the same-host socket applies (I136).
func ApplyDefaults(cfg *config.Daemon) error {
	if cfg.DataDir == "" {
		return errors.New("daemon configuration: dataDir is required")
	}
	if !filepath.IsAbs(cfg.DataDir) {
		return errors.New("daemon configuration: dataDir must be absolute")
	}
	if cfg.SpoolDir == "" {
		cfg.SpoolDir = filepath.Join(cfg.DataDir, "spool")
	}
	if cfg.ScratchDir == "" {
		cfg.ScratchDir = filepath.Join(cfg.DataDir, "scratch")
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = filepath.Join(cfg.DataDir, "cache")
	}
	if cfg.Listeners.Unix == nil && cfg.Listeners.HTTPS == nil {
		path, err := defaultSocketPath()
		if err != nil {
			return err
		}
		cfg.Listeners.Unix = &config.UnixListener{Path: path}
	}
	return nil
}

// defaultSocketPath matches the CLI's default (I162).
func defaultSocketPath() (string, error) {
	switch runtime.GOOS {
	case "linux":
		return "/run/vidyom/vidyomd.sock", nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the default socket: %w", err)
		}
		return filepath.Join(home, ".vidyom", "vidyomd.sock"), nil
	default:
		return "", fmt.Errorf("no default listener on %s: configure listeners.https with a loopback bind", runtime.GOOS)
	}
}

// ValidateConfig checks what `vidyomd start` needs, before anything is opened.
func ValidateConfig(cfg config.Daemon) error {
	if cfg.Vault.Unseal.Provider == "" {
		return errors.New("no master key provider: set vault.unseal.provider in daemon.json; the daemon does not start without one (D13)")
	}
	if cfg.Redis.URL == "" {
		return errors.New("no job queue: set redis.url in daemon.json; Redis is required (D4)")
	}
	for name, dir := range map[string]string{"dataDir": cfg.DataDir, "spoolDir": cfg.SpoolDir, "scratchDir": cfg.ScratchDir, "cacheDir": cfg.CacheDir} {
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("daemon configuration: %s must be absolute", name)
		}
	}
	for _, root := range cfg.IngestRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("daemon configuration: ingest root %q must be absolute", root)
		}
	}
	for _, user := range cfg.LocalUsers {
		if user.UID < 0 || user.Email == "" {
			return fmt.Errorf("daemon configuration: local user %d needs a non-negative uid and an email", user.UID)
		}
	}
	return validateListeners(cfg.Listeners)
}
