// Package provenance is how a scratch file keeps its origin (I185): a small sidecar
// recording the storage role, key, checksum and profile it came from. It lets
// video.cut accept a local copy derived from the master and nothing else (§6.4), and
// audio.extract refuse a file derived from the proxy (D56). No client input can
// write one, and the worker never reads one from a location outside its scratch.
package provenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"

	"github.com/rafaelp/vidyom/internal/contract/domain"
)

const suffix = ".origin.json"

// Origin says where a file came from.
type Origin struct {
	Role    domain.StorageRole `json:"role,omitempty"`
	Key     string             `json:"key,omitempty"`
	SHA256  string             `json:"sha256,omitempty"`
	Profile string             `json:"profile,omitempty"` // id@version
	Family  string             `json:"family,omitempty"`  // profile family
}

// Write records the origin of the file at path.
func Write(path string, o Origin) error {
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return os.WriteFile(path+suffix, data, 0o600)
}

// Read returns the recorded origin of the file at path. A file without one reports
// false. A sidecar that cannot be parsed is an error, never an empty origin.
func Read(path string) (Origin, bool, error) {
	data, err := os.ReadFile(path + suffix)
	if errors.Is(err, fs.ErrNotExist) {
		return Origin{}, false, nil
	}
	if err != nil {
		return Origin{}, false, err
	}
	var o Origin
	if err := json.Unmarshal(data, &o); err != nil {
		return Origin{}, false, err
	}
	return o, true, nil
}

// HashFile returns a file's SHA-256 and size, stopping when ctx ends.
func HashFile(ctx context.Context, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			size += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return hex.EncodeToString(h.Sum(nil)), size, nil
		}
		if err != nil {
			return "", 0, err
		}
	}
}
