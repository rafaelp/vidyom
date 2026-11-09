package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	gokeyring "github.com/zalando/go-keyring"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

// MasterKeySize is the size of the master key and of every data key.
const MasterKeySize = 32

// Names the providers read (I10, I111).
const (
	EnvMasterKey       = "VIDYOM_MASTER_KEY_B64"
	EnvInsecureDev     = "VIDYOM_INSECURE_DEV_UNSEAL"
	EnvCredentialsDir  = "CREDENTIALS_DIRECTORY"
	DefaultItemName    = "vidyom-master-key"
	keychainService    = "vidyom"
	insecureDevEnabled = "1"
)

// Seams for tests. Production code never changes them.
var (
	currentOS    = runtime.GOOS
	keychainRead = gokeyring.Get
)

// ProviderFor returns the provider named in daemon.json. There is no default: an
// absent or unknown name, or a provider missing a required option, is an error
// (D13, I10).
func ProviderFor(u config.Unseal) (security.MasterKeyProvider, error) {
	switch u.Provider {
	case "":
		return nil, errs.New(errs.CodeSealed, "vault.unseal.provider is not set; refusing to start without a master key provider (D13)")
	case config.UnsealFile:
		if u.Path == "" {
			return nil, errs.New(errs.CodeInvalidArgument, "the file master key provider needs vault.unseal.path")
		}
		if !u.AcknowledgeFileSealed {
			return nil, errs.New(errs.CodeFailedPrecondition, "the file master key provider keeps the key in plaintext on the host; set vault.unseal.acknowledgeFileSealed to accept that")
		}
		return fileProvider{path: u.Path}, nil
	case config.UnsealEnv:
		return envProvider{}, nil
	case config.UnsealSystemdCreds:
		item, err := itemName(u.ItemName)
		if err != nil {
			return nil, err
		}
		return systemdProvider{item: item}, nil
	case config.UnsealMacOSKeychain:
		item, err := itemName(u.ItemName)
		if err != nil {
			return nil, err
		}
		return keychainProvider{item: item}, nil
	case config.UnsealDPAPI:
		if u.Path == "" {
			return nil, errs.New(errs.CodeInvalidArgument, "the dpapi master key provider needs vault.unseal.path")
		}
		return dpapiProvider{path: u.Path}, nil
	case config.UnsealGCPKMS:
		return kmsProvider{}, nil
	default:
		return nil, errs.Newf(errs.CodeInvalidArgument, "unknown master key provider %q", u.Provider)
	}
}

func itemName(name string) (string, error) {
	if name == "" {
		return DefaultItemName, nil
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", errs.New(errs.CodeInvalidArgument, "vault.unseal.itemName must be a plain name")
	}
	return name, nil
}

// parseKey accepts exactly 32 raw bytes, or their standard base64 with optional
// surrounding whitespace (I111). Errors never echo the input.
func parseKey(data []byte) ([]byte, error) {
	if len(data) == MasterKeySize {
		return append([]byte(nil), data...), nil
	}
	text := bytes.TrimSpace(data)
	key := make([]byte, base64.StdEncoding.DecodedLen(len(text)))
	n, err := base64.StdEncoding.Decode(key, text)
	if err != nil || n != MasterKeySize {
		return nil, errors.New("the master key must be 32 bytes, raw or base64-encoded")
	}
	return key[:n], nil
}

// ---- file: Compose secrets, below D13's target and explicit about it ----

type fileProvider struct{ path string }

func (p fileProvider) Name() string                   { return string(config.UnsealFile) }
func (p fileProvider) Sealing() security.SealingLevel { return security.SealingFile }

func (p fileProvider) Unseal(context.Context) ([]byte, error) {
	info, err := os.Stat(p.path)
	if err != nil {
		return nil, errs.Wrap(errs.CodeSealed, err, "cannot read the master key file")
	}
	if !info.Mode().IsRegular() {
		return nil, errs.New(errs.CodeSealed, "the master key path is not a regular file")
	}
	if perm := info.Mode().Perm(); currentOS != "windows" && perm != 0o400 && perm != 0o440 {
		return nil, errs.Newf(errs.CodeSealed, "the master key file has mode %04o; it must be 0400 or 0440", perm)
	}
	data, err := os.ReadFile(p.path)
	if err != nil {
		return nil, errs.Wrap(errs.CodeSealed, err, "cannot read the master key file")
	}
	key, err := parseKey(data)
	if err != nil {
		return nil, errs.Wrap(errs.CodeSealed, err, err.Error())
	}
	return key, nil
}

// ---- env: development only, and it needs saying twice ----

type envProvider struct{}

func (envProvider) Name() string                   { return string(config.UnsealEnv) }
func (envProvider) Sealing() security.SealingLevel { return security.SealingDev }

// Unseal removes the key from the environment, so processes the daemon starts, such
// as plugins, never inherit it. A second Unseal therefore fails.
func (envProvider) Unseal(context.Context) ([]byte, error) {
	if os.Getenv(EnvInsecureDev) != insecureDevEnabled {
		return nil, errs.Newf(errs.CodeSealed, "the env master key provider is for development and needs %s=1", EnvInsecureDev)
	}
	value, ok := os.LookupEnv(EnvMasterKey)
	if !ok {
		return nil, errs.Newf(errs.CodeSealed, "%s is not set", EnvMasterKey)
	}
	key, err := parseKey([]byte(value))
	if err != nil {
		return nil, errs.Wrap(errs.CodeSealed, err, err.Error())
	}
	if err := os.Unsetenv(EnvMasterKey); err != nil {
		return nil, errs.Wrap(errs.CodeSealed, err, "cannot clear the master key from the environment")
	}
	return key, nil
}

// ---- systemd-creds: a credential systemd decrypted for this service ----

type systemdProvider struct{ item string }

func (p systemdProvider) Name() string { return string(config.UnsealSystemdCreds) }

// Sealing reports hardware: a credential not sealed with a TPM is the operator's
// choice and cannot be detected from inside the service (I111). Validate.
func (p systemdProvider) Sealing() security.SealingLevel { return security.SealingHardware }

func (p systemdProvider) Unseal(context.Context) ([]byte, error) {
	dir := os.Getenv(EnvCredentialsDir)
	if dir == "" {
		return nil, errs.Newf(errs.CodeSealed, "%s is not set: run the daemon with LoadCredentialEncrypted=%s", EnvCredentialsDir, p.item)
	}
	data, err := os.ReadFile(filepath.Join(dir, p.item))
	if err != nil {
		return nil, errs.Wrap(errs.CodeSealed, err, "cannot read the systemd credential")
	}
	key, err := parseKey(data)
	if err != nil {
		return nil, errs.Wrap(errs.CodeSealed, err, err.Error())
	}
	return key, nil
}

// ---- macos-keychain ----

type keychainProvider struct{ item string }

func (p keychainProvider) Name() string                   { return string(config.UnsealMacOSKeychain) }
func (p keychainProvider) Sealing() security.SealingLevel { return security.SealingOS }

func (p keychainProvider) Unseal(context.Context) ([]byte, error) {
	if currentOS != "darwin" {
		return nil, errs.New(errs.CodeSealed, "the macos-keychain master key provider only works on macOS")
	}
	value, err := keychainRead(keychainService, p.item)
	if err != nil {
		return nil, errs.Wrap(errs.CodeSealed, err, "cannot read the master key from the macOS Keychain")
	}
	key, err := parseKey([]byte(value))
	if err != nil {
		return nil, errs.Wrap(errs.CodeSealed, err, err.Error())
	}
	return key, nil
}

// ---- dpapi: compile-checked only (I10, I12). Validate. ----

type dpapiProvider struct{ path string }

func (p dpapiProvider) Name() string                   { return string(config.UnsealDPAPI) }
func (p dpapiProvider) Sealing() security.SealingLevel { return security.SealingOS }

func (p dpapiProvider) Unseal(context.Context) ([]byte, error) {
	blob, err := os.ReadFile(p.path)
	if err != nil {
		return nil, errs.Wrap(errs.CodeSealed, err, "cannot read the DPAPI-protected master key")
	}
	plain, err := dpapiDecrypt(blob)
	if err != nil {
		return nil, errs.Wrap(errs.CodeSealed, err, "cannot decrypt the master key with DPAPI")
	}
	key, err := parseKey(plain)
	if err != nil {
		return nil, errs.Wrap(errs.CodeSealed, err, err.Error())
	}
	return key, nil
}

// ---- gcp-kms: interface only (I10). Validate. ----

type kmsProvider struct{}

func (kmsProvider) Name() string                   { return string(config.UnsealGCPKMS) }
func (kmsProvider) Sealing() security.SealingLevel { return security.SealingKMS }

func (kmsProvider) Unseal(context.Context) ([]byte, error) {
	return nil, errs.NotImplemented("the gcp-kms master key provider")
}

// GenerateMasterKeyFile creates a new master key at path, base64 encoded, mode 0400.
// It refuses to overwrite: losing a master key loses every secret (I111). It backs
// `vidyomd init --generate-master-key`; nothing creates a key at startup (I10).
func GenerateMasterKeyFile(path string) error {
	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("generate master key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errs.New(errs.CodeConflict, "a master key file already exists at that path; refusing to overwrite it")
		}
		return fmt.Errorf("create master key file: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if _, err := f.WriteString(encoded + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write master key file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("sync master key file: %w", err)
	}
	return f.Close()
}
