package local

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rafaelp/vidyom/connectors/storage/internal/storageutil"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Driver is this connector's name in storages.json.
const Driver = "local"

const (
	driverVersion = "1"
	internalDir   = ".vidyom"
	metaDir       = internalDir + "/meta"
	gensDir       = internalDir + "/gens"
	tmpDir        = internalDir + "/tmp"
	partSuffix    = ".part"
	// staleTemp is how long an abandoned partial write survives.
	staleTemp = 7 * 24 * time.Hour
	copyBuf   = 1 << 20
)

// Capability names accepted by the disableCapabilities parameter.
const (
	capRange      = "range_reads"
	capCopy       = "server_side_copy"
	capResumable  = "resumable_upload"
	capChecksum   = "checksum_verification"
	capVersioning = "versioning"
	capDirect     = "direct_access"
)

var knownCapabilities = []string{capRange, capCopy, capResumable, capChecksum, capVersioning, capDirect}

type parameters struct {
	AccessLog           string   `json:"accessLog"`
	Versioning          *bool    `json:"versioning"`
	DisableCapabilities []string `json:"disableCapabilities"`
}

// Info describes the driver for the registry and GET /connectors.
func Info() connector.Info {
	return connector.Info{
		Family:       connector.FamilyStorage,
		Driver:       Driver,
		Version:      driverVersion,
		Available:    true,
		Capabilities: storageutil.CapabilityMap(fullCapabilities(true, config.UsageOperational, nil)),
	}
}

// Register adds the driver to a registry.
func Register(r connector.Registrar) { r.RegisterStorage(Info(), Open) }

func fullCapabilities(versioning bool, usage config.StorageUsage, disabled []string) connector.StorageCapabilities {
	has := func(name string) bool { return !slices.Contains(disabled, name) }
	return connector.StorageCapabilities{
		Versioning:           versioning && has(capVersioning),
		RangeReads:           has(capRange),
		ServerSideCopy:       has(capCopy),
		ResumableUpload:      has(capResumable),
		ChecksumVerification: has(capChecksum),
		// Storage classes and restore have no meaning on one disk (I187).
		StorageClasses:   false,
		RestoreSemantics: false,
		DirectAccess:     storageutil.DirectAccess(usage) && has(capDirect),
	}
}

// Storage is one role's instance for one tenant.
type Storage struct {
	root     *os.Root
	rootPath string
	role     domain.StorageRole
	tenant   domain.TenantID
	base     string // "<prefix>/<tenant>/", slash separated
	caps     connector.StorageCapabilities
	class    string
	log      *accessLog
	now      func() time.Time
}

var (
	_ connector.Storage      = (*Storage)(nil)
	_ connector.LinkProvider = (*Storage)(nil)
)

// Open opens an instance. The root must be absolute, and it is created if missing.
func Open(_ context.Context, p connector.StorageParams) (connector.Storage, error) {
	return open(p)
}

func open(p connector.StorageParams) (*Storage, error) {
	inst := p.Instance
	if !filepath.IsAbs(inst.Root) {
		return nil, errs.Newf(errs.CodeInvalidArgument, "local storage for role %s needs an absolute root", p.Role)
	}
	base, err := storageutil.Base(inst.Prefix, p.Tenant)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInvalidArgument, err, "invalid local storage namespace")
	}
	var params parameters
	if len(inst.Parameters) > 0 {
		dec := json.NewDecoder(bytes.NewReader(inst.Parameters))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&params); err != nil {
			return nil, errs.Wrap(errs.CodeInvalidArgument, err, "invalid local storage parameters")
		}
	}
	for _, name := range params.DisableCapabilities {
		if !slices.Contains(knownCapabilities, name) {
			return nil, errs.Newf(errs.CodeInvalidArgument, "local storage cannot disable capability %q", name)
		}
	}
	versioning := params.Versioning == nil || *params.Versioning
	if err := os.MkdirAll(inst.Root, 0o700); err != nil {
		return nil, errs.Wrap(errs.CodeUnavailable, err, "cannot create local storage root")
	}
	root, err := os.OpenRoot(inst.Root)
	if err != nil {
		return nil, errs.Wrap(errs.CodeUnavailable, err, "cannot open local storage root")
	}
	if err := root.MkdirAll(filepath.FromSlash(tmpDir), 0o700); err != nil {
		_ = root.Close()
		return nil, errs.Wrap(errs.CodeUnavailable, err, "cannot prepare local storage root")
	}
	s := &Storage{
		root:     root,
		rootPath: inst.Root,
		role:     p.Role,
		tenant:   p.Tenant,
		base:     base,
		caps:     fullCapabilities(versioning, inst.Usage, params.DisableCapabilities),
		class:    inst.StorageClass,
		now:      time.Now,
	}
	if params.AccessLog != "" {
		if !filepath.IsAbs(params.AccessLog) {
			_ = root.Close()
			return nil, errs.Newf(errs.CodeInvalidArgument, "the local storage access log needs an absolute path")
		}
		s.log, err = openAccessLog(params.AccessLog)
		if err != nil {
			_ = root.Close()
			return nil, errs.Wrap(errs.CodeUnavailable, err, "cannot open the local storage access log")
		}
	}
	s.removeStaleTemps()
	return s, nil
}

// Close releases the root and the log.
func (s *Storage) Close() error {
	err := s.root.Close()
	if s.log != nil {
		err = errors.Join(err, s.log.close())
	}
	return err
}

// Info describes this instance.
func (s *Storage) Info() connector.Info {
	info := Info()
	info.Capabilities = storageutil.CapabilityMap(s.caps)
	return info
}

// Role returns the instance's role.
func (s *Storage) Role() domain.StorageRole { return s.role }

// Capabilities declares what this instance offers.
func (s *Storage) Capabilities() connector.StorageCapabilities { return s.caps }

func unsupported(what string) error {
	return errs.Newf(errs.CodeCapabilityUnsupported, "this local storage instance declares no %s", what)
}

// meta is the record kept beside an object.
type meta struct {
	SHA256       string    `json:"sha256"`
	SizeBytes    int64     `json:"size_bytes"`
	ContentType  string    `json:"content_type,omitempty"`
	StorageClass string    `json:"storage_class,omitempty"`
	Generation   string    `json:"generation"`
	WrittenAt    time.Time `json:"written_at"`
}

func (s *Storage) dataPath(key string) string { return filepath.FromSlash(s.base + key) }
func (s *Storage) metaPath(key string) string {
	return filepath.FromSlash(metaDir + "/" + s.base + key + ".json")
}
func (s *Storage) gensPath(key string) string {
	return filepath.FromSlash(gensDir + "/" + s.base + key)
}

func validKey(key string) error {
	if err := domain.ValidateStorageKey(key); err != nil {
		return errs.Wrap(errs.CodeInvalidArgument, err, "invalid storage key")
	}
	return nil
}

func (s *Storage) mapError(err error, what string) error {
	var coded *errs.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &coded):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return errs.NotFound("object")
	default:
		return errs.Wrap(errs.CodeUnavailable, err, what+" failed on local storage")
	}
}

func (s *Storage) readMeta(key string) (meta, bool) {
	data, err := s.root.ReadFile(s.metaPath(key))
	if err != nil {
		return meta{}, false
	}
	var m meta
	if json.Unmarshal(data, &m) != nil {
		return meta{}, false
	}
	return m, true
}

func (s *Storage) info(key string, st fs.FileInfo, m meta, known bool) connector.ObjectInfo {
	out := connector.ObjectInfo{
		Key:       key,
		SizeBytes: st.Size(),
		ModTime:   st.ModTime(),
		Location:  domain.Location{Driver: Driver, Bucket: s.rootPath, Prefix: s.base},
	}
	// A record that disagrees with the file is ignored, not trusted.
	if known && m.SizeBytes == st.Size() {
		out.SHA256 = m.SHA256
		out.ContentType = m.ContentType
		out.Location.Generation = m.Generation
	}
	return out
}

// Stat describes an object without reading it.
func (s *Storage) Stat(_ context.Context, key string) (connector.ObjectInfo, error) {
	if err := validKey(key); err != nil {
		return connector.ObjectInfo{}, err
	}
	st, err := s.root.Stat(s.dataPath(key))
	if err != nil || st.IsDir() {
		if err == nil {
			err = fs.ErrNotExist
		}
		return connector.ObjectInfo{}, s.mapError(err, "stat")
	}
	if err := s.record("stat", classMetadata, key, 0); err != nil {
		return connector.ObjectInfo{}, err
	}
	m, known := s.readMeta(key)
	return s.info(key, st, m, known), nil
}

// Exists reports whether an object exists.
func (s *Storage) Exists(_ context.Context, key string) (bool, error) {
	if err := validKey(key); err != nil {
		return false, err
	}
	st, err := s.root.Stat(s.dataPath(key))
	missing := errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
	if err != nil && !missing {
		return false, s.mapError(err, "exists")
	}
	if err := s.record("exists", classMetadata, key, 0); err != nil {
		return false, err
	}
	return !missing && !st.IsDir(), nil
}

// Get opens an object for reading.
func (s *Storage) Get(_ context.Context, key string) (io.ReadCloser, connector.ObjectInfo, error) {
	if err := validKey(key); err != nil {
		return nil, connector.ObjectInfo{}, err
	}
	f, st, err := s.openFile(s.dataPath(key))
	if err != nil {
		return nil, connector.ObjectInfo{}, s.mapError(err, "get")
	}
	if err := s.record("get", classRead, key, st.Size()); err != nil {
		_ = f.Close()
		return nil, connector.ObjectInfo{}, err
	}
	m, known := s.readMeta(key)
	return f, s.info(key, st, m, known), nil
}

func (s *Storage) openFile(p string) (*os.File, fs.FileInfo, error) {
	f, err := s.root.Open(p)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if st.IsDir() {
		_ = f.Close()
		return nil, nil, fs.ErrNotExist
	}
	return f, st, nil
}

// DownloadRange reads length bytes from offset; a negative length reads to the end.
func (s *Storage) DownloadRange(_ context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if !s.caps.RangeReads {
		return nil, unsupported("range reads")
	}
	if err := validKey(key); err != nil {
		return nil, err
	}
	f, st, err := s.openFile(s.dataPath(key))
	if err != nil {
		return nil, s.mapError(err, "range read")
	}
	if offset < 0 || offset > st.Size() {
		_ = f.Close()
		return nil, errs.Newf(errs.CodeInvalidArgument, "range offset %d is outside the object", offset)
	}
	remaining := st.Size() - offset
	if length < 0 || length > remaining {
		length = remaining
	}
	if err := s.record("range", classRead, key, length); err != nil {
		_ = f.Close()
		return nil, err
	}
	return sectionCloser{io.NewSectionReader(f, offset, length), f}, nil
}

type sectionCloser struct {
	io.Reader
	io.Closer
}

// tempFile is a write in progress, owned by one Put: no two writers ever share one.
type tempFile struct {
	f      *os.File
	path   string
	hash   hash.Hash
	parked string // where a failed write leaves its bytes for a retry; empty for none
}

func (t *tempFile) discard(s *Storage) {
	_ = t.f.Close()
	_ = s.root.Remove(t.path)
}

// park keeps a failed write's bytes for the next attempt with the same idempotency
// key. A write that cannot be resumed is simply discarded.
func (t *tempFile) park(s *Storage) {
	if t.parked == "" {
		t.discard(s)
		return
	}
	_ = t.f.Close()
	if err := s.root.Rename(t.path, t.parked); err != nil {
		_ = s.root.Remove(t.path)
	}
}

// Put writes an object atomically. A wrong checksum or size fails and leaves no
// object. With a checksum to meet, the temp file is read back from disk before the
// rename, so what is verified is what was stored (I186). A write with an idempotency
// key and a seekable source resumes a partial file left by an earlier attempt.
func (s *Storage) Put(ctx context.Context, key string, r io.Reader, opts connector.PutOptions) (connector.ObjectInfo, error) {
	if err := validKey(key); err != nil {
		return connector.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return connector.ObjectInfo{}, err
	}
	tmp, err := s.openTemp(key, r, opts)
	if err != nil {
		return connector.ObjectInfo{}, s.mapError(err, "put")
	}
	sum, size, err := s.writeTemp(ctx, tmp, r, opts)
	if err != nil {
		tmp.park(s)
		return connector.ObjectInfo{}, s.mapError(err, "put")
	}
	if err := s.checkWritten(tmp, sum, size, opts); err != nil {
		tmp.discard(s)
		return connector.ObjectInfo{}, err
	}
	class := opts.StorageClass
	if class == "" {
		class = s.class
	}
	m := meta{
		SHA256:       sum,
		SizeBytes:    size,
		ContentType:  opts.ContentType,
		StorageClass: class,
		Generation:   s.newGeneration(),
		WrittenAt:    s.now().UTC(),
	}
	if err := tmp.f.Close(); err != nil {
		_ = s.root.Remove(tmp.path)
		return connector.ObjectInfo{}, s.mapError(err, "put")
	}
	if err := s.commit(tmp.path, key, m); err != nil {
		_ = s.root.Remove(tmp.path)
		if errors.Is(err, errNoRecord) {
			// A failed write leaves no object. An overwritten one is in its generations.
			_ = s.root.Remove(s.dataPath(key))
		}
		return connector.ObjectInfo{}, s.mapError(err, "put")
	}
	if err := s.record("put", classWrite, key, size); err != nil {
		return connector.ObjectInfo{}, err
	}
	return s.objectInfo(key, m)
}

func (s *Storage) objectInfo(key string, m meta) (connector.ObjectInfo, error) {
	st, err := s.root.Stat(s.dataPath(key))
	if err != nil {
		return connector.ObjectInfo{}, s.mapError(err, "stat")
	}
	return s.info(key, st, m, true), nil
}

func (s *Storage) newGeneration() string {
	return strconv.FormatInt(s.now().UnixNano(), 10)
}

func (s *Storage) openTemp(key string, r io.Reader, opts connector.PutOptions) (*tempFile, error) {
	if seeker, ok := r.(io.ReadSeeker); ok && opts.IdempotencyKey != "" && s.caps.ResumableUpload {
		return s.openResumable(key, seeker, opts)
	}
	f, p, err := s.createTemp("put-", partSuffix)
	if err != nil {
		return nil, err
	}
	return &tempFile{f: f, path: p, hash: sha256.New()}, nil
}

// createTemp makes a new file in the temp directory.
func (s *Storage) createTemp(prefix, suffix string) (*os.File, string, error) {
	for range 16 {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		p := filepath.Join(filepath.FromSlash(tmpDir), prefix+hex.EncodeToString(random[:])+suffix)
		f, err := s.root.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return f, p, nil
	}
	return nil, "", errors.New("cannot create a temp file")
}

// openResumable continues a parked partial file when its bytes match the source's
// start. Taking the parked file is a rename, so only one attempt can get it, and two
// overlapping attempts never write the same inode. A stale writer could otherwise keep
// appending to a file another attempt had already published. A write cut off by a
// crash stays under its private name and is swept later, so resuming covers failed
// attempts, not a killed process (I187).
func (s *Storage) openResumable(key string, r io.ReadSeeker, opts connector.PutOptions) (*tempFile, error) {
	name := sha256.Sum256([]byte(s.base + key + "\x00" + opts.IdempotencyKey))
	parked := filepath.Join(filepath.FromSlash(tmpDir), hex.EncodeToString(name[:])+partSuffix)
	f, p, err := s.createTemp("put-", partSuffix)
	if err != nil {
		return nil, err
	}
	tmp := &tempFile{f: f, path: p, hash: sha256.New(), parked: parked}
	// A successful rename replaces the empty file made above with the parked bytes.
	if err := s.root.Rename(parked, p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return tmp, nil
		}
		tmp.discard(s)
		return nil, err
	}
	_ = f.Close()
	f, err = s.root.OpenFile(p, os.O_RDWR, 0o600)
	if err != nil {
		_ = s.root.Remove(p)
		return nil, err
	}
	tmp.f = f
	st, err := f.Stat()
	if err != nil {
		tmp.discard(s)
		return nil, err
	}
	have := st.Size()
	if have == 0 {
		return tmp, nil
	}
	match, err := sameStart(f, r, have, tmp.hash)
	if err != nil {
		tmp.discard(s)
		return nil, err
	}
	if match {
		// Both files now sit at offset have, and the hash covers those bytes.
		return tmp, nil
	}
	tmp.hash.Reset()
	if err := f.Truncate(0); err != nil {
		tmp.discard(s)
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		tmp.discard(s)
		return nil, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		tmp.discard(s)
		return nil, err
	}
	return tmp, nil
}

// sameStart compares the first n bytes of the partial file and the source,
// feeding h. It leaves both positioned at n when they match.
func sameStart(partial io.Reader, source io.Reader, n int64, h hash.Hash) (bool, error) {
	a := make([]byte, copyBuf)
	b := make([]byte, copyBuf)
	for n > 0 {
		chunk := min(n, copyBuf)
		if _, err := io.ReadFull(partial, a[:chunk]); err != nil {
			return false, err
		}
		if _, err := io.ReadFull(source, b[:chunk]); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, err
		}
		if !bytes.Equal(a[:chunk], b[:chunk]) {
			return false, nil
		}
		h.Write(a[:chunk])
		n -= chunk
	}
	return true, nil
}

func (s *Storage) writeTemp(ctx context.Context, tmp *tempFile, r io.Reader, opts connector.PutOptions) (string, int64, error) {
	st, err := tmp.f.Stat()
	if err != nil {
		return "", 0, err
	}
	start := st.Size()
	progress := &storageutil.ProgressReader{R: storageutil.ContextReader{Ctx: ctx, R: r}, Done: start, Progress: opts.Progress}
	written, err := io.CopyBuffer(io.MultiWriter(tmp.f, tmp.hash), progress, make([]byte, copyBuf))
	if err != nil {
		return "", 0, err
	}
	if err := tmp.f.Sync(); err != nil {
		return "", 0, err
	}
	if opts.Progress != nil && written == 0 {
		opts.Progress(start)
	}
	return hex.EncodeToString(tmp.hash.Sum(nil)), start + written, nil
}

// checkWritten compares what was written with what was asked, reading the bytes
// back from the file when a checksum was given.
func (s *Storage) checkWritten(tmp *tempFile, sum string, size int64, opts connector.PutOptions) error {
	if opts.SizeBytes > 0 && opts.SizeBytes != size {
		return errs.Newf(errs.CodeValidationFailed, "size mismatch writing an object: got %d bytes, expected %d", size, opts.SizeBytes)
	}
	if opts.SHA256 == "" {
		return nil
	}
	if !strings.EqualFold(opts.SHA256, sum) {
		return errs.New(errs.CodeValidationFailed, "checksum mismatch writing an object")
	}
	if _, err := tmp.f.Seek(0, io.SeekStart); err != nil {
		return s.mapError(err, "verify")
	}
	h := sha256.New()
	if _, err := io.CopyBuffer(h, tmp.f, make([]byte, copyBuf)); err != nil {
		return s.mapError(err, "verify")
	}
	if !strings.EqualFold(opts.SHA256, hex.EncodeToString(h.Sum(nil))) {
		return errs.New(errs.CodeValidationFailed, "checksum mismatch reading back a written object")
	}
	return nil
}

// commitLocks serializes commits of one key in this process, so an object and its
// record cannot interleave with another writer's.
var commitLocks [64]sync.Mutex

func (s *Storage) lockFor(key string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s.rootPath + "\x00" + s.base + key))
	return &commitLocks[h.Sum32()%uint32(len(commitLocks))]
}

// errNoRecord says an object reached its key and its record did not.
var errNoRecord = errors.New("the object's record could not be written")

// commit moves a finished file into place with its record. The rename replaces the
// old object atomically, so readers see one or the other and a failure keeps the old
// one. The old record goes first, so a crash leaves an object with no checksum, never
// a wrong one, and a failed rename puts it back.
func (s *Storage) commit(srcPath, key string, m meta) error {
	lock := s.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	dst := s.dataPath(key)
	if err := s.root.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return errs.Wrap(errs.CodeInvalidArgument, err, "this key cannot be stored: part of its path is an object")
	}
	if err := s.keepGeneration(key); err != nil {
		return err
	}
	metaPath := s.metaPath(key)
	oldRecord, readErr := s.root.ReadFile(metaPath)
	if err := s.root.Remove(metaPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := s.root.Rename(srcPath, dst); err != nil {
		if readErr == nil {
			_ = s.root.WriteFile(metaPath, oldRecord, 0o600)
		}
		return err
	}
	if err := s.syncDir(filepath.Dir(dst)); err != nil {
		return err
	}
	if err := s.writeMeta(key, m); err != nil {
		return fmt.Errorf("%w: %w", errNoRecord, err)
	}
	return nil
}

// keepGeneration adds the current object to the generations when versioning is on.
// It links rather than moves: the key must keep answering until the new object takes
// its place, and a crash here must not leave the key empty.
func (s *Storage) keepGeneration(key string) error {
	if !s.caps.Versioning {
		return nil
	}
	dst := s.dataPath(key)
	st, err := s.root.Stat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.IsDir() {
		return errs.New(errs.CodeInvalidArgument, "this key cannot be stored: an object lives under it")
	}
	m, known := s.readMeta(key)
	if !known || m.Generation == "" {
		m.Generation = s.newGeneration()
		m.SizeBytes = st.Size()
	}
	dir := s.gensPath(key)
	if err := s.root.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	target := filepath.Join(dir, m.Generation)
	if err := s.root.Link(dst, target); err != nil && !errors.Is(err, fs.ErrExist) {
		// Some file systems have no hard links.
		if err := s.copyWithin(dst, target); err != nil {
			return err
		}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.root.WriteFile(target+".json", data, 0o600)
}

func (s *Storage) copyWithin(src, dst string) error {
	in, err := s.root.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := s.root.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.CopyBuffer(out, in, make([]byte, copyBuf)); err != nil {
		_ = out.Close()
		_ = s.root.Remove(dst)
		return err
	}
	return out.Close()
}

func (s *Storage) writeMeta(key string, m meta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p := s.metaPath(key)
	if err := s.root.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, tmp, err := s.createTemp("meta-", ".tmp")
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = s.root.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = s.root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = s.root.Remove(tmp)
		return err
	}
	if err := s.root.Rename(tmp, p); err != nil {
		_ = s.root.Remove(tmp)
		return err
	}
	return s.syncDir(filepath.Dir(p))
}

func (s *Storage) syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := s.root.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// List pages objects in key order under a key prefix.
func (s *Storage) List(_ context.Context, opts connector.ListOptions) (connector.ListPage, error) {
	if err := s.record("list", classMetadata, opts.Prefix, 0); err != nil {
		return connector.ListPage{}, err
	}
	keys, err := s.keys(opts.Prefix)
	if err != nil {
		return connector.ListPage{}, s.mapError(err, "list")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 1000
	}
	start := 0
	if opts.Cursor != "" {
		start = sort.SearchStrings(keys, opts.Cursor)
		if start < len(keys) && keys[start] == opts.Cursor {
			start++
		}
	}
	var page connector.ListPage
	for _, key := range keys[start:] {
		if len(page.Objects) == limit {
			page.NextCursor = page.Objects[limit-1].Key
			break
		}
		st, err := s.root.Stat(s.dataPath(key))
		if err != nil {
			continue
		}
		m, known := s.readMeta(key)
		page.Objects = append(page.Objects, s.info(key, st, m, known))
	}
	return page, nil
}

func (s *Storage) keys(prefix string) ([]string, error) {
	base := strings.TrimSuffix(s.base, "/")
	var keys []string
	err := fs.WalkDir(s.root.FS(), base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		key := strings.TrimPrefix(p, s.base)
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

// Delete removes an object, its record and its generations.
func (s *Storage) Delete(_ context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	lock := s.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	dst := s.dataPath(key)
	st, err := s.root.Stat(dst)
	if err != nil || st.IsDir() {
		if err == nil {
			err = fs.ErrNotExist
		}
		return s.mapError(err, "delete")
	}
	if err := s.root.Remove(dst); err != nil {
		return s.mapError(err, "delete")
	}
	_ = s.root.Remove(s.metaPath(key))
	_ = s.root.RemoveAll(s.gensPath(key))
	s.pruneParents(key)
	return s.record("delete", classWrite, key, st.Size())
}

// pruneParents removes directories a deleted object left empty, up to the tenant's
// root. Remove fails on a directory with entries, which ends the walk.
func (s *Storage) pruneParents(key string) {
	for dir := path.Dir(key); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if s.root.Remove(filepath.FromSlash(s.base+dir)) != nil {
			return
		}
	}
}

// Copy duplicates an object inside this instance, verified against its record.
func (s *Storage) Copy(ctx context.Context, srcKey, dstKey string) (connector.ObjectInfo, error) {
	if !s.caps.ServerSideCopy {
		return connector.ObjectInfo{}, unsupported("server-side copy")
	}
	if err := validKey(srcKey); err != nil {
		return connector.ObjectInfo{}, err
	}
	if err := validKey(dstKey); err != nil {
		return connector.ObjectInfo{}, err
	}
	f, st, err := s.openFile(s.dataPath(srcKey))
	if err != nil {
		return connector.ObjectInfo{}, s.mapError(err, "copy")
	}
	defer func() { _ = f.Close() }()
	m, known := s.readMeta(srcKey)
	opts := connector.PutOptions{SizeBytes: st.Size()}
	if known && m.SizeBytes == st.Size() {
		opts.SHA256 = m.SHA256
		opts.ContentType = m.ContentType
		opts.StorageClass = m.StorageClass
	}
	// A copy goes through Put's checks, and logs as a write under the destination.
	info, err := s.putQuiet(ctx, dstKey, f, opts)
	if err != nil {
		return connector.ObjectInfo{}, err
	}
	if err := s.record("copy", classWrite, dstKey, info.SizeBytes); err != nil {
		return connector.ObjectInfo{}, err
	}
	return info, nil
}

// putQuiet is Put without its log line, for operations that log their own.
func (s *Storage) putQuiet(ctx context.Context, key string, r io.Reader, opts connector.PutOptions) (connector.ObjectInfo, error) {
	quiet := *s
	quiet.log = nil
	return quiet.Put(ctx, key, r, opts)
}

// Move renames an object inside this instance.
func (s *Storage) Move(ctx context.Context, srcKey, dstKey string) (connector.ObjectInfo, error) {
	if !s.caps.ServerSideCopy {
		return connector.ObjectInfo{}, unsupported("server-side copy")
	}
	if err := validKey(srcKey); err != nil {
		return connector.ObjectInfo{}, err
	}
	if err := validKey(dstKey); err != nil {
		return connector.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return connector.ObjectInfo{}, err
	}
	src := s.dataPath(srcKey)
	st, err := s.root.Stat(src)
	if err != nil || st.IsDir() {
		if err == nil {
			err = fs.ErrNotExist
		}
		return connector.ObjectInfo{}, s.mapError(err, "move")
	}
	m, known := s.readMeta(srcKey)
	if !known || m.SizeBytes != st.Size() {
		sum, err := s.hashObject(ctx, src)
		if err != nil {
			return connector.ObjectInfo{}, s.mapError(err, "move")
		}
		m = meta{SHA256: sum, SizeBytes: st.Size()}
	}
	m.Generation = s.newGeneration()
	m.WrittenAt = s.now().UTC()
	if err := s.commit(src, dstKey, m); err != nil {
		return connector.ObjectInfo{}, s.mapError(err, "move")
	}
	_ = s.root.Remove(s.metaPath(srcKey))
	_ = s.root.RemoveAll(s.gensPath(srcKey))
	s.pruneParents(srcKey)
	if err := s.record("move", classWrite, dstKey, m.SizeBytes); err != nil {
		return connector.ObjectInfo{}, err
	}
	return s.objectInfo(dstKey, m)
}

func (s *Storage) hashObject(ctx context.Context, p string) (string, error) {
	f, err := s.root.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, storageutil.ContextReader{Ctx: ctx, R: f}, make([]byte, copyBuf)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Checksum computes the object's SHA-256 from its bytes.
func (s *Storage) Checksum(ctx context.Context, key string) (string, error) {
	if !s.caps.ChecksumVerification {
		return "", unsupported("checksum verification")
	}
	if err := validKey(key); err != nil {
		return "", err
	}
	sum, err := s.hashObject(ctx, s.dataPath(key))
	if err != nil {
		return "", s.mapError(err, "checksum")
	}
	if err := s.record("checksum", classVerify, key, 0); err != nil {
		return "", err
	}
	return sum, nil
}

// Verify recomputes the object's checksum and compares it.
func (s *Storage) Verify(ctx context.Context, key, wantSHA256 string) (connector.VerifyResult, error) {
	if !s.caps.ChecksumVerification {
		return connector.VerifyResult{}, unsupported("checksum verification")
	}
	if err := validKey(key); err != nil {
		return connector.VerifyResult{}, err
	}
	st, err := s.root.Stat(s.dataPath(key))
	if err != nil {
		return connector.VerifyResult{}, s.mapError(err, "verify")
	}
	sum, err := s.hashObject(ctx, s.dataPath(key))
	if err != nil {
		return connector.VerifyResult{}, s.mapError(err, "verify")
	}
	if err := s.record("verify", classVerify, key, st.Size()); err != nil {
		return connector.VerifyResult{}, err
	}
	return connector.VerifyResult{OK: strings.EqualFold(sum, wantSHA256), SHA256: sum, SizeBytes: st.Size()}, nil
}

// TemporaryLink returns a file:// URL for review on the same host. Nothing can
// enforce its expiry: access is the file system's (I198).
func (s *Storage) TemporaryLink(ctx context.Context, key string, _ time.Duration) (string, error) {
	if _, err := s.Stat(ctx, key); err != nil {
		return "", err
	}
	abs := filepath.Join(s.rootPath, s.dataPath(key))
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String(), nil
}

// Generations lists the kept earlier generations of a key, oldest first.
func (s *Storage) Generations(_ context.Context, key string) ([]string, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(s.root.FS(), s.base0(gensDir, key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, s.mapError(err, "generations")
	}
	var out []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			out = append(out, entry.Name())
		}
	}
	sort.Slice(out, func(a, b int) bool {
		x, _ := strconv.ParseInt(out[a], 10, 64)
		y, _ := strconv.ParseInt(out[b], 10, 64)
		return x < y
	})
	return out, nil
}

func (s *Storage) base0(dir, key string) string { return dir + "/" + s.base + key }

// GetGeneration opens a kept earlier generation.
func (s *Storage) GetGeneration(_ context.Context, key, generation string) (io.ReadCloser, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	if !safeGeneration(generation) {
		return nil, errs.Newf(errs.CodeInvalidArgument, "invalid generation %q", generation)
	}
	f, st, err := s.openFile(filepath.Join(s.gensPath(key), generation))
	if err != nil {
		return nil, s.mapError(err, "get generation")
	}
	if err := s.record("get", classRead, key, st.Size()); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func safeGeneration(g string) bool {
	if g == "" {
		return false
	}
	for _, c := range g {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (s *Storage) removeStaleTemps() {
	dir := filepath.FromSlash(tmpDir)
	entries, err := fs.ReadDir(s.root.FS(), tmpDir)
	if err != nil {
		return
	}
	cutoff := s.now().Add(-staleTemp)
	for _, entry := range entries {
		info, err := entry.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = s.root.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

func (s *Storage) record(op string, class accessClass, key string, bytes int64) error {
	if s.log == nil {
		return nil
	}
	if err := s.log.write(accessEntry{
		At: s.now().UTC(), Tenant: s.tenant, Role: s.role, Op: op, Class: class, Key: key, Bytes: bytes,
	}); err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, fmt.Sprintf("cannot write the access log for %s", op))
	}
	return nil
}
