package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// implicitServer is the name of the same-host server used when nothing was
// configured (I162).
const implicitServer = "local"

// defaultRemoteSocket is where an SSH host's daemon listens unless told otherwise.
const defaultRemoteSocket = "/run/vidyom/vidyomd.sock"

// serverEntry is one configured server. It never holds a credential (I160).
type serverEntry struct {
	Kind apiclient.Kind `json:"kind"`

	Socket string `json:"socket,omitempty"`

	URL         string `json:"url,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	CAFile      string `json:"ca_file,omitempty"`
	ClientCert  string `json:"client_cert,omitempty"`
	ClientKey   string `json:"client_key,omitempty"`

	SSHTarget    string `json:"ssh_target,omitempty"`
	RemoteSocket string `json:"remote_socket,omitempty"`

	Tenant      string `json:"tenant,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`
	DeviceEmail string `json:"device_email,omitempty"`

	// ClientSecrets lists names of client-location secrets per tenant (I177). The
	// values are in the OS keyring.
	ClientSecrets map[string][]string `json:"client_secrets,omitempty"`
}

func (s *serverEntry) endpoint() apiclient.Endpoint {
	return apiclient.Endpoint{
		Kind: s.Kind, Socket: s.Socket, URL: s.URL, Fingerprint: s.Fingerprint,
		CAFile: s.CAFile, ClientCert: s.ClientCert, ClientKey: s.ClientKey,
		SSHTarget: s.SSHTarget, RemoteSocket: s.RemoteSocket,
	}
}

// sameEndpoint reports whether a credential issued by one entry may stay with the
// other: same transport to the same place (I163).
func (s *serverEntry) sameEndpoint(o *serverEntry) bool {
	return s.Kind == o.Kind && s.Socket == o.Socket && s.URL == o.URL && s.SSHTarget == o.SSHTarget &&
		s.RemoteSocket == o.RemoteSocket && s.Fingerprint == o.Fingerprint
}

// clientConfig is <config-dir>/config.json.
type clientConfig struct {
	Version int                     `json:"version"`
	Current string                  `json:"current_server,omitempty"`
	Servers map[string]*serverEntry `json:"servers,omitempty"`
}

const configVersion = 1

func configFile(dir string) string { return filepath.Join(dir, "config.json") }

func loadConfig(dir string) (*clientConfig, error) {
	raw, err := os.ReadFile(configFile(dir))
	if errors.Is(err, os.ErrNotExist) {
		return &clientConfig{Version: configVersion, Servers: map[string]*serverEntry{}}, nil
	}
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot read the client configuration")
	}
	var cfg clientConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, errs.Wrap(errs.CodeInvalidArgument, err, "the client configuration is not valid: "+configFile(dir))
	}
	if cfg.Version > configVersion {
		return nil, errs.Newf(errs.CodeFailedPrecondition, "the client configuration was written by a newer vidyom (version %d)", cfg.Version)
	}
	if cfg.Servers == nil {
		cfg.Servers = map[string]*serverEntry{}
	}
	return &cfg, nil
}

// save writes through a temporary file and a rename, mode 0600 in a 0700 directory.
func (c *clientConfig) save(dir string) error {
	c.Version = configVersion
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot create the client configuration directory")
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot encode the client configuration")
	}
	tmp, err := os.CreateTemp(dir, "config-*")
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot write the client configuration")
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errs.Wrap(errs.CodeInternal, err, "cannot write the client configuration")
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return errs.Wrap(errs.CodeInternal, err, "cannot write the client configuration")
	}
	if err := tmp.Close(); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot write the client configuration")
	}
	if err := os.Rename(tmp.Name(), configFile(dir)); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot write the client configuration")
	}
	return nil
}

func (c *clientConfig) names() []string {
	names := make([]string, 0, len(c.Servers))
	for name := range c.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var serverNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func validateServerName(name string) error {
	if !serverNamePattern.MatchString(name) {
		return usagef("invalid server name %q: use letters, digits, '.', '_' and '-'", name)
	}
	return nil
}

// defaultSocket is the platform's same-host socket (I162, I136). Empty means none:
// Windows reaches its daemon through HTTPS on loopback (I44).
func defaultSocket(home func() (string, error)) string {
	switch runtime.GOOS {
	case "linux":
		return defaultRemoteSocket
	case "windows":
		return ""
	default:
		dir, err := home()
		if err != nil || dir == "" {
			return ""
		}
		return filepath.Join(dir, ".vidyom", "vidyomd.sock")
	}
}

// configDir resolves <config-dir> (I160).
func configDir(explicit string, getenv func(string) string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if dir := getenv("VIDYOM_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot find a configuration directory, set VIDYOM_CONFIG_DIR: %w", err)
	}
	return filepath.Join(base, "vidyom"), nil
}
