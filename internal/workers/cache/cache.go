// Package cache is the worker's per-tenant cache of stored objects (§6.4, D22,
// I39, I189). Entries are encrypted at rest with chunked AES-256-GCM under a key the
// daemon derives for the tenant and hands over per lease. Reuse decrypts into a
// caller-chosen plaintext file while recomputing SHA-256, so the checksum is
// verified on every reuse, and the sealed header's role and origin checksum are
// compared with the request. Eviction uses only file sizes and modification times,
// so sweeps need no key.
package cache

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

const (
	magic          = "VCACHE1\n"
	saltSize       = 32
	entrySuffix    = ".vcache"
	tempSuffix     = ".tmp"
	fileKeyInfo    = "vidyom-cache-file-v1"
	defaultChunk   = 1 << 20
	maxChunk       = 64 << 20
	tagOverhead    = 16
	staleTemp      = 24 * time.Hour
	lengthPrefix   = 4
	headerChunkIdx = 0
)

// DefaultMaxBytes bounds a tenant's cache when configuration says nothing (I189).
const DefaultMaxBytes int64 = 50 << 30

// Options configures a Cache.
type Options struct {
	// Root is the cache directory; each tenant gets its own subdirectory (§14.1).
	Root string
	// MaxBytes bounds each tenant's entries. Zero means DefaultMaxBytes.
	MaxBytes int64
	// IdleExpiry removes entries unused this long. Zero means the 7 days of D22.
	IdleExpiry time.Duration
	// ChunkSize is the plaintext size of a chunk. Zero means 1 MiB.
	ChunkSize int
	Now       func() time.Time
	Logger    *slog.Logger
}

// Cache is the cache of one worker process, or of several sharing a directory.
type Cache struct {
	opts  Options
	locks entryLocks
}

// New prepares the cache directory.
func New(opts Options) (*Cache, error) {
	if opts.Root == "" {
		return nil, errors.New("cache: no directory")
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.IdleExpiry <= 0 {
		opts.IdleExpiry = config.DefaultCacheIdleDays * 24 * time.Hour
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = defaultChunk
	}
	if opts.ChunkSize > maxChunk {
		return nil, errors.New("cache: chunk size too large")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := os.MkdirAll(opts.Root, 0o700); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	return &Cache{opts: opts}, nil
}

// Tenant is one tenant's view of the cache, usable only with that tenant's key.
type Tenant struct {
	cache  *Cache
	tenant domain.TenantID
	key    []byte
	dir    string
}

// ForTenant opens a tenant's directory with its key. The key stays in memory; a
// key from another tenant finds nothing and can authenticate nothing.
func (c *Cache) ForTenant(tenant domain.TenantID, key []byte) (*Tenant, error) {
	if err := tenant.Validate(); err != nil {
		return nil, errs.Wrap(errs.CodeInvalidArgument, err, "invalid tenant")
	}
	if len(key) != 32 {
		return nil, errs.New(errs.CodeFailedPrecondition, "the cache key must be 32 bytes")
	}
	dir := filepath.Join(c.opts.Root, string(tenant))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errs.Wrap(errs.CodeUnavailable, err, "cannot prepare the tenant cache")
	}
	return &Tenant{cache: c, tenant: tenant, key: append([]byte(nil), key...), dir: dir}, nil
}

// Request names one stored object.
type Request struct {
	Role   domain.StorageRole
	Key    string
	SHA256 string // the origin's checksum, authoritative
}

// Result reports what Fetch delivered.
type Result struct {
	Hit       bool
	SHA256    string
	SizeBytes int64
}

// Download opens the object from its origin.
type Download func(ctx context.Context) (io.ReadCloser, error)

// header is sealed as the first chunk of an entry.
type header struct {
	Role   domain.StorageRole `json:"role"`
	Key    string             `json:"key"`
	SHA256 string             `json:"sha256"`
}

// Fetch delivers the object as plaintext at dst, from a verified entry or from the
// origin. A stale or damaged entry counts as a miss and is removed. A download whose
// checksum differs from req.SHA256 fails and installs nothing.
func (t *Tenant) Fetch(ctx context.Context, req Request, dst string, download Download) (Result, error) {
	if req.SHA256 == "" {
		return Result{}, errs.New(errs.CodeFailedPrecondition, "a cached object needs its origin checksum")
	}
	id := t.entryID(req)
	unlock, err := t.cache.locks.lock(ctx, id)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	path := filepath.Join(t.dir, id+entrySuffix)
	size, err := t.readEntry(ctx, path, req, dst)
	switch {
	case err == nil:
		now := t.cache.opts.Now()
		_ = os.Chtimes(path, now, now)
		return Result{Hit: true, SHA256: strings.ToLower(req.SHA256), SizeBytes: size}, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return Result{}, err
	case errors.Is(err, fs.ErrNotExist):
	default:
		// Tampered, truncated, written under another key, or unreadable: drop it.
		t.cache.opts.Logger.Warn("dropping a damaged cache entry", "tenant", string(t.tenant), "error", err)
		_ = os.Remove(path)
	}
	size, err = t.fill(ctx, path, req, dst, download)
	if err != nil {
		return Result{}, err
	}
	if err := t.sweepDir(t.dir); err != nil {
		t.cache.opts.Logger.Warn("cache sweep failed", "tenant", string(t.tenant), "error", err)
	}
	return Result{SHA256: strings.ToLower(req.SHA256), SizeBytes: size}, nil
}

func (t *Tenant) entryID(req Request) string {
	mac := hmac.New(sha256.New, t.key)
	_, _ = mac.Write([]byte(string(req.Role) + "\x00" + req.Key + "\x00" + strings.ToLower(req.SHA256)))
	return hex.EncodeToString(mac.Sum(nil))
}

func (t *Tenant) fileCipher(salt []byte) (cipher.AEAD, error) {
	fileKey, err := hkdf.Key(sha256.New, t.key, salt, fileKeyInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(fileKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// chunkNonce is the STREAM nonce: the chunk's index and whether it is the last. A
// file's key is unique through its salt, so the counter never repeats under a key.
func chunkNonce(index uint64, final bool) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[:8], index)
	if final {
		nonce[11] = 1
	}
	return nonce
}

func aad(salt []byte) []byte { return append([]byte(magic), salt...) }

// fill downloads, encrypts into a temp file while writing plaintext to dst, then
// installs the entry only if the checksum matches.
func (t *Tenant) fill(ctx context.Context, path string, req Request, dst string, download Download) (int64, error) {
	body, err := download(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = body.Close() }()
	plain, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, errs.Wrap(errs.CodeUnavailable, err, "cannot create the local copy")
	}
	temp, err := os.CreateTemp(t.dir, "fill-*"+tempSuffix)
	if err != nil {
		_ = plain.Close()
		return 0, errs.Wrap(errs.CodeUnavailable, err, "cannot create a cache entry")
	}
	abort := func() {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
		_ = plain.Close()
		_ = os.Remove(dst)
	}
	size, sum, err := t.encryptStream(ctx, temp, plain, body, req)
	if err != nil {
		abort()
		return 0, err
	}
	if !strings.EqualFold(sum, req.SHA256) {
		abort()
		return 0, errs.New(errs.CodeUnavailable, "checksum mismatch downloading an object")
	}
	if err := plain.Close(); err != nil {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
		_ = os.Remove(dst)
		return 0, errs.Wrap(errs.CodeUnavailable, err, "cannot finish the local copy")
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
		return size, nil // the copy is good; only the entry is lost
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(temp.Name())
		return size, nil
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		_ = os.Remove(temp.Name())
		return size, nil
	}
	now := t.cache.opts.Now()
	_ = os.Chtimes(path, now, now)
	return size, nil
}

// encryptStream writes the entry format: magic, salt, then length-prefixed sealed
// chunks. The first chunk is the header, the last has the final flag. The reader of
// the source is always one chunk ahead so the last chunk can be flagged.
func (t *Tenant) encryptStream(ctx context.Context, out io.Writer, plain io.Writer, body io.Reader, req Request) (int64, string, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return 0, "", err
	}
	aead, err := t.fileCipher(salt)
	if err != nil {
		return 0, "", err
	}
	w := bufio.NewWriterSize(out, 1<<16)
	if _, err := w.WriteString(magic); err != nil {
		return 0, "", cacheWriteError(err)
	}
	if _, err := w.Write(salt); err != nil {
		return 0, "", cacheWriteError(err)
	}
	additional := aad(salt)
	head, err := json.Marshal(header{Role: req.Role, Key: req.Key, SHA256: strings.ToLower(req.SHA256)})
	if err != nil {
		return 0, "", err
	}
	if err := writeChunk(w, aead, headerChunkIdx, false, head, additional); err != nil {
		return 0, "", cacheWriteError(err)
	}

	hash := sha256.New()
	var total int64
	index := uint64(headerChunkIdx + 1)
	current := make([]byte, t.cache.opts.ChunkSize)
	next := make([]byte, t.cache.opts.ChunkSize)
	currentLen, err := readChunk(ctx, body, current)
	if err != nil {
		return 0, "", err
	}
	for {
		nextLen, err := readChunk(ctx, body, next)
		if err != nil {
			return 0, "", err
		}
		final := nextLen == 0
		hash.Write(current[:currentLen])
		if _, err := plain.Write(current[:currentLen]); err != nil {
			return 0, "", errs.Wrap(errs.CodeUnavailable, err, "cannot write the local copy")
		}
		if err := writeChunk(w, aead, index, final, current[:currentLen], additional); err != nil {
			return 0, "", cacheWriteError(err)
		}
		total += int64(currentLen)
		index++
		if final {
			break
		}
		current, next = next, current
		currentLen = nextLen
	}
	if err := w.Flush(); err != nil {
		return 0, "", cacheWriteError(err)
	}
	return total, hex.EncodeToString(hash.Sum(nil)), nil
}

func cacheWriteError(err error) error {
	return errs.Wrap(errs.CodeUnavailable, err, "cannot write a cache entry")
}

func readChunk(ctx context.Context, r io.Reader, buf []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(r, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return n, nil
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, errs.Wrap(errs.CodeUnavailable, err, "the object download failed")
	}
	return n, nil
}

func writeChunk(w io.Writer, aead cipher.AEAD, index uint64, final bool, plain, additional []byte) error {
	sealed := aead.Seal(nil, chunkNonce(index, final), plain, additional)
	var length [lengthPrefix]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(sealed)))
	if _, err := w.Write(length[:]); err != nil {
		return err
	}
	_, err := w.Write(sealed)
	return err
}

var errDamaged = errors.New("cache entry failed authentication")

// readEntry decrypts an entry to dst, verifying everything. On any failure dst is
// removed.
func (t *Tenant) readEntry(ctx context.Context, path string, req Request, dst string) (size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1<<16)
	var prefix [len(magic) + saltSize]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return 0, fmt.Errorf("%w: short prefix", errDamaged)
	}
	if string(prefix[:len(magic)]) != magic {
		return 0, fmt.Errorf("%w: bad magic", errDamaged)
	}
	salt := prefix[len(magic):]
	aead, err := t.fileCipher(salt)
	if err != nil {
		return 0, err
	}
	additional := aad(salt)

	headPlain, _, more, err := openChunk(r, aead, headerChunkIdx, additional)
	if err != nil {
		return 0, err
	}
	if !more {
		return 0, fmt.Errorf("%w: header without content", errDamaged)
	}
	var head header
	if err := json.Unmarshal(headPlain, &head); err != nil {
		return 0, fmt.Errorf("%w: bad header", errDamaged)
	}
	if head.Role != req.Role || head.Key != req.Key || !strings.EqualFold(head.SHA256, req.SHA256) {
		return 0, fmt.Errorf("%w: header names another object", errDamaged)
	}

	plain, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, errs.Wrap(errs.CodeUnavailable, err, "cannot create the local copy")
	}
	defer func() {
		closeErr := plain.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	hash := sha256.New()
	index := uint64(headerChunkIdx + 1)
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		chunk, final, _, err := openChunk(r, aead, index, additional)
		if err != nil {
			return 0, err
		}
		hash.Write(chunk)
		if _, err := plain.Write(chunk); err != nil {
			return 0, errs.Wrap(errs.CodeUnavailable, err, "cannot write the local copy")
		}
		size += int64(len(chunk))
		index++
		if final {
			break
		}
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), req.SHA256) {
		return 0, fmt.Errorf("%w: checksum differs from the origin", errDamaged)
	}
	return size, nil
}

// openChunk reads and authenticates one chunk. Whether it is the last depends on
// whether bytes follow, and the final flag is in the nonce: cutting a file at a
// chunk boundary makes its new last chunk fail authentication.
func openChunk(r *bufio.Reader, aead cipher.AEAD, index uint64, additional []byte) (plain []byte, final, more bool, err error) {
	var length [lengthPrefix]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return nil, false, false, fmt.Errorf("%w: short chunk length", errDamaged)
	}
	n := binary.BigEndian.Uint32(length[:])
	if n < tagOverhead || n > maxChunk+tagOverhead {
		return nil, false, false, fmt.Errorf("%w: bad chunk length", errDamaged)
	}
	sealed := make([]byte, n)
	if _, err := io.ReadFull(r, sealed); err != nil {
		return nil, false, false, fmt.Errorf("%w: short chunk", errDamaged)
	}
	_, peekErr := r.Peek(1)
	final = errors.Is(peekErr, io.EOF)
	plain, err = aead.Open(nil, chunkNonce(index, final), sealed, additional)
	if err != nil {
		return nil, false, false, fmt.Errorf("%w: chunk %d", errDamaged, index)
	}
	return plain, final, !final, nil
}

// Sweep expires idle entries and enforces the size limit in every tenant's
// directory. It needs no key, because it reads only sizes and times.
func (c *Cache) Sweep() error {
	entries, err := os.ReadDir(c.opts.Root)
	if err != nil {
		return err
	}
	var all []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if domain.TenantID(entry.Name()).Validate() != nil {
			continue
		}
		t := &Tenant{cache: c}
		all = append(all, t.sweepDir(filepath.Join(c.opts.Root, entry.Name())))
	}
	return errors.Join(all...)
}

type sweepEntry struct {
	path string
	size int64
	used time.Time
}

func (t *Tenant) sweepDir(dir string) error {
	c := t.cache
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	now := c.opts.Now()
	var live []sweepEntry
	var total int64
	var all []error
	for _, file := range files {
		info, err := file.Info()
		if err != nil || file.IsDir() {
			continue
		}
		path := filepath.Join(dir, file.Name())
		switch {
		case strings.HasSuffix(file.Name(), tempSuffix):
			if now.Sub(info.ModTime()) > staleTemp {
				_ = os.Remove(path)
			}
		case strings.HasSuffix(file.Name(), entrySuffix):
			if now.Sub(info.ModTime()) > c.opts.IdleExpiry {
				if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
					all = append(all, err)
				}
				continue
			}
			live = append(live, sweepEntry{path, info.Size(), info.ModTime()})
			total += info.Size()
		}
	}
	if total <= c.opts.MaxBytes {
		return errors.Join(all...)
	}
	sort.Slice(live, func(a, b int) bool { return live[a].used.Before(live[b].used) })
	for _, entry := range live {
		if total <= c.opts.MaxBytes {
			break
		}
		if err := os.Remove(entry.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			all = append(all, err)
			continue
		}
		total -= entry.size
	}
	return errors.Join(all...)
}

// entryLocks serializes work on one entry in this process, so a second task for the
// same object waits and then reuses what the first downloaded.
type entryLocks struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func (l *entryLocks) lock(ctx context.Context, id string) (func(), error) {
	for {
		l.mu.Lock()
		if l.locks == nil {
			l.locks = map[string]chan struct{}{}
		}
		held, busy := l.locks[id]
		if !busy {
			release := make(chan struct{})
			l.locks[id] = release
			l.mu.Unlock()
			return func() {
				l.mu.Lock()
				delete(l.locks, id)
				l.mu.Unlock()
				close(release)
			}, nil
		}
		l.mu.Unlock()
		select {
		case <-held:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
