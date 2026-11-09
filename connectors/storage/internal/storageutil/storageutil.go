// Package storageutil holds what the storage drivers share.
package storageutil

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// CapabilityMap renders capabilities for connector.Info.
func CapabilityMap(c connector.StorageCapabilities) map[string]bool {
	return map[string]bool{
		"versioning":            c.Versioning,
		"range_reads":           c.RangeReads,
		"server_side_copy":      c.ServerSideCopy,
		"resumable_upload":      c.ResumableUpload,
		"checksum_verification": c.ChecksumVerification,
		"storage_classes":       c.StorageClasses,
		"restore_semantics":     c.RestoreSemantics,
		"direct_access":         c.DirectAccess,
	}
}

// DirectAccess applies §7.2: the policy comes from the instance's declared usage,
// never from the driver. An undeclared usage is not direct (I187).
func DirectAccess(usage config.StorageUsage) bool { return usage == config.UsageOperational }

// NormalizePrefix cleans an instance prefix to "a/b/" or "". It refuses anything
// that could climb out of the instance's location.
func NormalizePrefix(prefix string) (string, error) {
	trimmed := strings.Trim(prefix, "/")
	if trimmed == "" {
		return "", nil
	}
	if strings.Contains(trimmed, "\\") || path.Clean(trimmed) != trimmed {
		return "", fmt.Errorf("invalid storage prefix %q", prefix)
	}
	for _, part := range strings.Split(trimmed, "/") {
		if part == ".." || part == "." {
			return "", fmt.Errorf("invalid storage prefix %q", prefix)
		}
	}
	return trimmed + "/", nil
}

// Base returns the namespace of one tenant inside an instance (D6): the instance
// prefix followed by the tenant prefix.
func Base(prefix string, tenant domain.TenantID) (string, error) {
	if err := tenant.Validate(); err != nil {
		return "", err
	}
	normalized, err := NormalizePrefix(prefix)
	if err != nil {
		return "", err
	}
	return normalized + config.TenantStoragePrefix(tenant), nil
}

// ContextReader stops a long copy when the context ends.
type ContextReader struct {
	Ctx context.Context
	R   io.Reader
}

func (c ContextReader) Read(p []byte) (int, error) {
	if err := c.Ctx.Err(); err != nil {
		return 0, err
	}
	return c.R.Read(p)
}

// ProgressReader reports the bytes read so far.
type ProgressReader struct {
	R        io.Reader
	Done     int64
	Progress func(bytesDone int64)
}

func (p *ProgressReader) Read(b []byte) (int, error) {
	n, err := p.R.Read(b)
	if n > 0 {
		p.Done += int64(n)
		if p.Progress != nil {
			p.Progress(p.Done)
		}
	}
	return n, err
}
