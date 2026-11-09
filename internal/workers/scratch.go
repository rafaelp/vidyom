package workers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// scratchArea implements node.Scratch for one task (I5). The tenant comes from the
// lease and never from a URI, so a reference cannot reach another tenant's files.
type scratchArea struct {
	tenantDir string // <scratch>/<tenant>
	job       id.ULID
	nodeID    string
	// ancestors are the jobs whose files this task may also read (I182).
	ancestors []id.ULID
}

var _ node.Scratch = (*scratchArea)(nil)

func checkScratchName(name string) error {
	if name == "" {
		return errs.New(errs.CodeInvalidArgument, "empty scratch file name")
	}
	for _, part := range strings.Split(name, "/") {
		if !safeName.MatchString(part) || part == "." || part == ".." {
			return errs.Newf(errs.CodeInvalidArgument, "invalid scratch file name %q", name)
		}
	}
	return nil
}

func (s *scratchArea) jobDir() string { return filepath.Join(s.tenantDir, string(s.job)) }

func (s *scratchArea) nodeDir() string { return filepath.Join(s.jobDir(), s.nodeID) }

// Path returns a path this node may write, creating parent directories.
func (s *scratchArea) Path(name string) (string, error) {
	if err := checkScratchName(name); err != nil {
		return "", err
	}
	path := filepath.Join(s.nodeDir(), filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", errs.Wrap(errs.CodeUnavailable, err, "cannot create a scratch directory")
	}
	return path, nil
}

// URI returns the scratch:// reference for a file written under Path.
func (s *scratchArea) URI(name string) string {
	return domain.InputURI{Kind: domain.URIScratch, JobULID: string(s.job), NodeID: s.nodeID, File: name}.String()
}

// Resolve turns a scratch:// reference of this job, or of an ancestor named by the
// lease, into a local path. Other jobs' files are refused, and a symlink cannot lead
// outside the tenant's scratch.
func (s *scratchArea) Resolve(uri domain.InputURI) (string, error) {
	if uri.Kind != domain.URIScratch {
		return "", errs.New(errs.CodeInvalidArgument, "not a scratch reference")
	}
	job := id.ULID(uri.JobULID)
	if job != s.job && !slices.Contains(s.ancestors, job) {
		return "", errs.New(errs.CodeForbidden, "this scratch reference belongs to another job")
	}
	if !id.IsULID(uri.JobULID) || !safeName.MatchString(uri.NodeID) {
		return "", errs.New(errs.CodeInvalidArgument, "invalid scratch reference")
	}
	if err := checkScratchName(uri.File); err != nil {
		return "", err
	}
	path := filepath.Join(s.tenantDir, uri.JobULID, uri.NodeID, filepath.FromSlash(uri.File))
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errs.NotFound("scratch file")
		}
		return "", errs.Wrap(errs.CodeUnavailable, err, "cannot resolve a scratch file")
	}
	realRoot, err := filepath.EvalSymlinks(s.tenantDir)
	if err != nil {
		return "", errs.Wrap(errs.CodeUnavailable, err, "cannot resolve the scratch directory")
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errs.New(errs.CodeForbidden, "this scratch reference leaves the tenant's scratch")
	}
	return real, nil
}

// touchJob marks a job as in use, so the idle reaper leaves it alone.
func touchJob(scratchDir string, tenant domain.TenantID, job id.ULID, now time.Time) (string, error) {
	if err := tenant.Validate(); err != nil {
		return "", errs.Wrap(errs.CodeInvalidArgument, err, "invalid tenant in a lease")
	}
	if !id.IsULID(string(job)) {
		return "", errs.Newf(errs.CodeInvalidArgument, "invalid job %q in a lease", job)
	}
	dir := filepath.Join(scratchDir, string(tenant), string(job))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errs.Wrap(errs.CodeUnavailable, err, "cannot create the job's scratch directory")
	}
	if err := os.Chtimes(dir, now, now); err != nil {
		return "", errs.Wrap(errs.CodeUnavailable, err, "cannot touch the job's scratch directory")
	}
	return dir, nil
}

// RemoveJobScratch deletes a finished job's scratch directory (I45, D22). A missing
// directory is success, since another worker on the volume may have done it.
func RemoveJobScratch(scratchDir string, tenant domain.TenantID, job id.ULID) error {
	if err := tenant.Validate(); err != nil {
		return fmt.Errorf("remove job scratch: %w", err)
	}
	if !id.IsULID(string(job)) {
		return fmt.Errorf("remove job scratch: invalid job %q", job)
	}
	return os.RemoveAll(filepath.Join(scratchDir, string(tenant), string(job)))
}

// reapIdleScratch removes job directories idle longer than maxIdle, skipping jobs
// that run here now (I181). It is the net under the cleanup pointer.
func reapIdleScratch(scratchDir string, maxIdle time.Duration, now time.Time, running func(id.ULID) bool) (removed int, err error) {
	tenants, err := os.ReadDir(scratchDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	for _, tenant := range tenants {
		if !tenant.IsDir() || domain.TenantID(tenant.Name()).Validate() != nil {
			continue
		}
		tenantDir := filepath.Join(scratchDir, tenant.Name())
		jobs, err := os.ReadDir(tenantDir)
		if err != nil {
			continue
		}
		for _, job := range jobs {
			if !job.IsDir() || !id.IsULID(job.Name()) || running(id.ULID(job.Name())) {
				continue
			}
			info, err := job.Info()
			if err != nil || now.Sub(newestIn(filepath.Join(tenantDir, job.Name()), info.ModTime())) <= maxIdle {
				continue
			}
			if err := os.RemoveAll(filepath.Join(tenantDir, job.Name())); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// newestIn finds the most recent modification among a job's node directories.
func newestIn(dir string, start time.Time) time.Time {
	newest := start
	entries, err := os.ReadDir(dir)
	if err != nil {
		return newest
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}
