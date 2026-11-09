package youtube

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/errs"
)

const sessionKeyInfo = "vidyom-youtube-session-v1"

// session is one resumable upload, per idempotency key. URI works as a bearer
// capability, so it is sealed on disk and never appears in an error or a log.
type session struct {
	URI       string    `json:"uri"`
	Size      int64     `json:"size"`
	VideoID   string    `json:"video_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// errSealedRecord means a record exists and this credential cannot read it.
var errSealedRecord = errors.New("youtube: a session record is sealed under another credential")

// sessionStore keeps sessions by idempotency key.
type sessionStore interface {
	load(key string) (*session, error)
	save(key string, s session) error
	// durable reports whether a missing record proves no session was ever opened: a
	// session is saved before its first byte (I227).
	durable() bool
}

// memoryStore lasts as long as the connector. After a restart nothing is known, and
// an abandoned session can never create a video.
type memoryStore struct {
	mu       sync.Mutex
	sessions map[string]session
}

func newMemoryStore() *memoryStore { return &memoryStore{sessions: map[string]session{}} }

func (m *memoryStore) load(key string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[key]
	if !ok {
		return nil, nil
	}
	return &s, nil
}

func (m *memoryStore) save(key string, s session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[key] = s
	return nil
}

func (m *memoryStore) durable() bool { return false }

// fileStore seals each session with AES-256-GCM under a key derived from the
// credential, with the idempotency key as additional data, so a record cannot be
// moved to another key and never sits in plaintext (I227).
type fileStore struct {
	dir string
	mu  sync.Mutex
	gcm cipher.AEAD
}

func newFileStore(dir string, credential []byte) (*fileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errs.Wrap(errs.CodeUnavailable, err, "cannot create the youtube state directory")
	}
	s := &fileStore{dir: dir}
	if err := s.rekey(credential); err != nil {
		return nil, err
	}
	return s, nil
}

func (f *fileStore) rekey(credential []byte) error {
	key, err := hkdf.Key(sha256.New, credential, nil, sessionKeyInfo, 32)
	if err != nil {
		return fmt.Errorf("derive the session key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("create the session cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("create the session cipher: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gcm = gcm
	return nil
}

func (f *fileStore) durable() bool { return true }

func (f *fileStore) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(f.dir, hex.EncodeToString(sum[:16])+".session")
}

func (f *fileStore) load(key string) (*session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := os.ReadFile(f.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errs.Wrap(errs.CodeUnavailable, err, "cannot read a youtube session record")
	}
	size := f.gcm.NonceSize()
	if len(data) < size {
		return nil, errSealedRecord
	}
	plain, err := f.gcm.Open(nil, data[:size], data[size:], []byte(key))
	if err != nil {
		return nil, errSealedRecord
	}
	var s session
	if err := json.Unmarshal(plain, &s); err != nil {
		return nil, errSealedRecord
	}
	return &s, nil
}

func (f *fileStore) save(key string, s session) error {
	plain, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode a session record: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	nonce := make([]byte, f.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("session nonce: %w", err)
	}
	sealed := f.gcm.Seal(nonce, nonce, plain, []byte(key))

	tmp, err := os.CreateTemp(f.dir, ".session.tmp-*")
	if err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot write a youtube session record")
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(sealed)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), f.path(key))
	}
	if err == nil && runtime.GOOS != "windows" { // Windows cannot flush a directory
		var dir *os.File
		if dir, err = os.Open(f.dir); err == nil {
			err = dir.Sync()
			dir.Close()
		}
	}
	if err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot write a youtube session record")
	}
	return nil
}
