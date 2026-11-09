package plugins

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
	"github.com/rafaelp/vidyom/schemas"
)

// PermJobTemp is the filesystem grant for a process's private temp directory (I264).
const PermJobTemp = "job-temp"

const readOnlyPrefix = "ro:"

var (
	// The schema only checks a version's prefix, and the version becomes part of a path.
	semverRe = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	eventRe  = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*$`)
	contract = regexp.MustCompile(`^([0-9]+)(\.(x|[0-9]+))?(\.(x|[0-9]+))?$`)
)

// knownContracts are the families a manifest may name, and the majors this build
// speaks (I271).
var knownContracts = map[string]int{
	"plugin":     plugin.ProtocolVersion,
	"hook":       1,
	"event":      1,
	"storage":    1,
	"publishing": 1,
	"catalog":    1,
	"log_sink":   1,
	"node":       1,
}

// ParseManifest checks a manifest document against schemas/plugin-manifest.v1.json and
// decodes it. Schema validity is not enough to install: see Validate.
func ParseManifest(data []byte) (plugin.Manifest, error) {
	var m plugin.Manifest
	if err := schemas.Validate(schemas.PluginManifest, data); err != nil {
		return m, errs.Wrap(errs.CodeValidationFailed, err, "plugin manifest does not match the schema: "+firstLine(err.Error()))
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, errs.Wrap(errs.CodeValidationFailed, err, "cannot decode the plugin manifest")
	}
	return m, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Validate applies the rules beyond the schema. What this version cannot activate is
// refused with not_implemented rather than accepted and ignored (I42, I269).
func Validate(m plugin.Manifest) error {
	if !semverRe.MatchString(m.Version) {
		return errs.Newf(errs.CodeValidationFailed, "plugin version %q is not a valid version", m.Version)
	}
	switch m.Runtime {
	case plugin.RuntimeProcess:
	case plugin.RuntimeRemote:
		return errs.NotImplemented("remote plugins over HTTPS")
	default:
		return errs.Newf(errs.CodeValidationFailed, "unknown plugin runtime %q", m.Runtime)
	}
	if err := validateProvides(m.Provides); err != nil {
		return err
	}
	if err := validateEntrypoint(m.Entrypoint); err != nil {
		return err
	}
	if err := validatePermissions(m.Permissions); err != nil {
		return err
	}
	for _, sub := range m.Provides.EventSubscriptions {
		if !slices.Contains(m.Permissions.Events, sub) {
			return errs.Newf(errs.CodeValidationFailed, "event subscription %q is not covered by permissions.events", sub)
		}
	}
	return validateContracts(m.Contracts)
}

func validateEntrypoint(e *plugin.Entrypoint) error {
	if e == nil {
		return errs.New(errs.CodeValidationFailed, "a process plugin needs an entrypoint")
	}
	command := e.Command
	if command == "" || strings.ContainsAny(command, "\\\x00") || filepath.IsAbs(command) || strings.HasPrefix(command, "/") {
		return errs.New(errs.CodeValidationFailed, "entrypoint command must be a path relative to the plugin directory")
	}
	// A bare name such as python3 is a file in the plugin directory, never a lookup on
	// PATH (I265), so checkTree refuses it unless the plugin ships such a file.
	if clean := filepath.ToSlash(filepath.Clean(command)); clean == ".." || strings.HasPrefix(clean, "../") {
		return errs.New(errs.CodeValidationFailed, "entrypoint command must stay inside the plugin directory")
	}
	for _, arg := range e.Args {
		if strings.ContainsRune(arg, 0) {
			return errs.New(errs.CodeValidationFailed, "entrypoint arguments cannot contain NUL")
		}
	}
	return nil
}

func validateProvides(p plugin.Provides) error {
	unsupported := []struct {
		field string
		items []string
	}{
		{"nodeTypes", p.NodeTypes},
		{"storageDrivers", p.StorageDrivers},
		{"publishingDrivers", p.PublishingDrivers},
		{"catalogDrivers", p.CatalogDrivers},
		{"logSinkDrivers", p.LogSinkDrivers},
	}
	for _, u := range unsupported {
		if len(u.items) > 0 {
			return errs.NotImplemented("a plugin providing " + u.field)
		}
	}
	if len(p.HookActions) == 0 && len(p.EventSubscriptions) == 0 {
		return errs.New(errs.CodeValidationFailed, "a plugin must provide a hook action or an event subscription")
	}
	for _, action := range p.HookActions {
		if !nameRe.MatchString(action) {
			return errs.Newf(errs.CodeValidationFailed, "invalid hook action name %q", action)
		}
	}
	for _, sub := range p.EventSubscriptions {
		if !eventRe.MatchString(sub) {
			return errs.Newf(errs.CodeValidationFailed, "invalid event subscription %q", sub)
		}
	}
	return nil
}

func validatePermissions(p plugin.Permissions) error {
	for _, rule := range p.Network {
		if _, err := parseNetworkRule(rule); err != nil {
			return err
		}
	}
	for _, name := range p.Secrets {
		if !nameRe.MatchString(name) {
			return errs.Newf(errs.CodeValidationFailed, "invalid secret name %q", name)
		}
	}
	for _, entry := range p.Filesystem {
		if _, _, err := parseFilesystemEntry(entry); err != nil {
			return err
		}
	}
	for _, name := range p.Events {
		if !eventRe.MatchString(name) {
			return errs.Newf(errs.CodeValidationFailed, "invalid event permission %q", name)
		}
	}
	return nil
}

// parseFilesystemEntry returns the path and whether it is read-only. job-temp returns
// an empty path.
func parseFilesystemEntry(entry string) (path string, readOnly bool, err error) {
	if entry == PermJobTemp {
		return "", false, nil
	}
	path = entry
	if strings.HasPrefix(entry, readOnlyPrefix) {
		path = strings.TrimPrefix(entry, readOnlyPrefix)
		readOnly = true
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", false, errs.Newf(errs.CodeValidationFailed, "filesystem permission %q must be job-temp or a clean absolute path", entry)
	}
	if path == string(filepath.Separator) {
		return "", false, errs.New(errs.CodeValidationFailed, "filesystem permission cannot be the root directory")
	}
	return path, readOnly, nil
}

func validateContracts(contracts map[string]string) error {
	for family, version := range contracts {
		major, known := knownContracts[family]
		if !known {
			return errs.Newf(errs.CodeValidationFailed, "unknown contract %q", family)
		}
		match := contract.FindStringSubmatch(version)
		if match == nil {
			return errs.Newf(errs.CodeValidationFailed, "contract %q has an invalid version %q", family, version)
		}
		got, err := strconv.Atoi(match[1])
		if err != nil || got != major {
			return errs.Newf(errs.CodeFailedPrecondition, "contract %q version %q is not compatible with this build's %d.x", family, version, major)
		}
	}
	return nil
}
