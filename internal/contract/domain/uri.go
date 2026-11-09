package domain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
)

// URIKind is the form of a workflow file reference (R20, D39, I5).
type URIKind string

const (
	URIServer  URIKind = "server"
	URIUpload  URIKind = "upload"
	URIStorage URIKind = "storage"
	URIImport  URIKind = "import"
	URIScratch URIKind = "scratch" // internal; rejected from user inputs
)

// InputURI is a parsed file reference. Only the fields of its Kind are set.
type InputURI struct {
	Kind URIKind

	Path      string      // server
	UploadRef string      // upload: UPL-nnn or ULID
	Role      StorageRole // storage
	Key       string      // storage
	Connector string      // import
	Remote    string      // import
	JobULID   string      // scratch
	NodeID    string      // scratch
	File      string      // scratch
}

var (
	safeSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	nodeID      = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// ValidateStorageKey rejects keys that could escape a role's namespace.
func ValidateStorageKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return fmt.Errorf("invalid storage key %q", key)
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid storage key %q", key)
		}
	}
	return nil
}

// ParseInputURI parses any reference form. A value with no recognized prefix is
// a server path, kept for compatibility with `--input file=/data/class.mp4` (R20).
func ParseInputURI(s string) (InputURI, error) {
	switch {
	case s == "":
		return InputURI{}, errors.New("empty input reference")
	case strings.HasPrefix(s, "server:"):
		return parseServer(strings.TrimPrefix(s, "server:"))
	case strings.HasPrefix(s, "upload:"):
		ref := strings.TrimPrefix(s, "upload:")
		if !id.IsFriendly(ref) && !id.IsULID(ref) {
			return InputURI{}, fmt.Errorf("invalid upload reference %q", s)
		}
		if id.IsFriendly(ref) && id.Friendly(ref).Prefix() != id.PrefixUpload {
			return InputURI{}, fmt.Errorf("invalid upload reference %q", s)
		}
		return InputURI{Kind: URIUpload, UploadRef: ref}, nil
	case strings.HasPrefix(s, "storage://"):
		role, key, ok := strings.Cut(strings.TrimPrefix(s, "storage://"), "/")
		if !ok || !StorageRole(role).Valid() {
			return InputURI{}, fmt.Errorf("invalid storage reference %q: expected storage://<role>/<key>", s)
		}
		if err := ValidateStorageKey(key); err != nil {
			return InputURI{}, err
		}
		return InputURI{Kind: URIStorage, Role: StorageRole(role), Key: key}, nil
	case strings.HasPrefix(s, "import:"):
		connector, remote, ok := strings.Cut(strings.TrimPrefix(s, "import:"), "/")
		if !ok || connector == "" || remote == "" {
			return InputURI{}, fmt.Errorf("invalid import reference %q: expected import:<connector>/<id>", s)
		}
		return InputURI{Kind: URIImport, Connector: connector, Remote: remote}, nil
	case strings.HasPrefix(s, "scratch://"):
		parts := strings.SplitN(strings.TrimPrefix(s, "scratch://"), "/", 3)
		if len(parts) != 3 || !id.IsULID(parts[0]) || !nodeID.MatchString(parts[1]) {
			return InputURI{}, fmt.Errorf("invalid scratch reference %q", s)
		}
		for _, part := range strings.Split(parts[2], "/") {
			if !safeSegment.MatchString(part) || part == "." || part == ".." {
				return InputURI{}, fmt.Errorf("invalid scratch reference %q", s)
			}
		}
		return InputURI{Kind: URIScratch, JobULID: parts[0], NodeID: parts[1], File: parts[2]}, nil
	default:
		return parseServer(s)
	}
}

func parseServer(p string) (InputURI, error) {
	if !filepath.IsAbs(p) {
		return InputURI{}, fmt.Errorf("server path %q must be absolute", p)
	}
	return InputURI{Kind: URIServer, Path: p}, nil
}

// ParseUserInputURI parses a reference coming from a client: the four forms of
// R20, never the internal scratch form.
func ParseUserInputURI(s string) (InputURI, error) {
	u, err := ParseInputURI(s)
	if err != nil {
		return InputURI{}, err
	}
	if u.Kind == URIScratch {
		return InputURI{}, errors.New("scratch references are internal and cannot be supplied by a client")
	}
	return u, nil
}

// String renders the canonical form.
func (u InputURI) String() string {
	switch u.Kind {
	case URIServer:
		return "server:" + u.Path
	case URIUpload:
		return "upload:" + u.UploadRef
	case URIStorage:
		return "storage://" + string(u.Role) + "/" + u.Key
	case URIImport:
		return "import:" + u.Connector + "/" + u.Remote
	case URIScratch:
		return "scratch://" + u.JobULID + "/" + u.NodeID + "/" + u.File
	default:
		return ""
	}
}

// ForbidRole fails when the reference points at one of the given storage roles.
// Clip rendering calls it with RoleArchive; there is no fallback (§3.2, §6.4).
func (u InputURI) ForbidRole(roles ...StorageRole) error {
	if u.Kind != URIStorage {
		return nil
	}
	for _, r := range roles {
		if u.Role == r {
			return errs.Newf(errs.CodeForbiddenStorageRole, "reading from the %s role is forbidden here", r)
		}
	}
	return nil
}

// RequireRole fails unless the reference is a stored object in the given role.
// A scratch artifact is accepted only when allowScratch is true: video.cut uses
// the worker's verified cache copy of a master, which it tracks by role (§6.4).
func (u InputURI) RequireRole(role StorageRole, allowScratch bool) error {
	if u.Kind == URIScratch && allowScratch {
		return nil
	}
	if u.Kind != URIStorage || u.Role != role {
		return errs.Newf(errs.CodeForbiddenStorageRole, "this operation only reads from the %s role", role)
	}
	return nil
}

// ResolveServerPath enforces ingestRoots for server: references (R20, §15.6):
// the path, with symlinks resolved, must stay inside one of the roots. It
// returns the resolved path.
func ResolveServerPath(roots []string, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errs.Newf(errs.CodeInvalidArgument, "server path must be absolute")
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return "", errs.Newf(errs.CodeInvalidArgument, "server path must not contain '..'")
		}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errs.NotFound("server path")
		}
		return "", errs.Wrap(errs.CodeInvalidArgument, err, "cannot resolve server path")
	}
	for _, root := range roots {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(realRoot, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	return "", errs.Newf(errs.CodeForbidden, "server path is outside the configured ingest roots")
}
