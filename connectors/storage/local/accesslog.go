package local

import (
	"encoding/json"
	"os"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
)

type accessClass string

// Access classes (I188). "No archive read" means no entry with role archive and
// class read: backing up a clip writes to archive, and that is not reading media.
const (
	classRead     accessClass = "read"
	classVerify   accessClass = "verify"
	classWrite    accessClass = "write"
	classMetadata accessClass = "metadata"
)

// accessEntry is one line of the access log.
type accessEntry struct {
	At     time.Time          `json:"at"`
	Tenant domain.TenantID    `json:"tenant"`
	Role   domain.StorageRole `json:"role"`
	Op     string             `json:"op"`
	Class  accessClass        `json:"class"`
	Key    string             `json:"key"`
	Bytes  int64              `json:"bytes"`
}

// accessLog appends JSON lines. Each entry is one write on an O_APPEND file, so
// several workers can share a file without interleaving lines.
type accessLog struct {
	f *os.File
}

func openAccessLog(path string) (*accessLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &accessLog{f: f}, nil
}

func (l *accessLog) write(e accessEntry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = l.f.Write(append(line, '\n'))
	return err
}

func (l *accessLog) close() error { return l.f.Close() }
