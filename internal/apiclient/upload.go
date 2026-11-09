package apiclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// maxChunkBytes bounds how much of a file is held in memory, whatever the server's
// record asks for (I172).
const maxChunkBytes = 256 << 20

// Checkpoint is what survives a killed client (I172). It holds no file content.
type Checkpoint struct {
	Path           string `json:"path"`
	SizeBytes      int64  `json:"size_bytes"`
	ModTimeUnixNs  int64  `json:"mod_time_unix_ns"`
	SHA256         string `json:"sha256"`
	IdempotencyKey string `json:"idempotency_key"`
	UploadID       string `json:"upload_id,omitempty"`
	UploadULID     string `json:"upload_ulid,omitempty"`
}

// CheckpointStore persists checkpoints by key.
type CheckpointStore interface {
	Load(key string) (*Checkpoint, error) // nil, nil when absent
	Save(key string, cp Checkpoint) error
	Delete(key string) error
}

// FileCheckpoints stores each checkpoint as a 0600 file in a 0700 directory,
// written through a rename so a kill never leaves a torn file.
type FileCheckpoints struct{ Dir string }

func (f FileCheckpoints) file(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(f.Dir, hex.EncodeToString(sum[:])+".json")
}

// Load implements CheckpointStore. A corrupt file counts as absent.
func (f FileCheckpoints) Load(key string) (*Checkpoint, error) {
	raw, err := os.ReadFile(f.file(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, nil
	}
	return &cp, nil
}

// Save implements CheckpointStore.
func (f FileCheckpoints) Save(key string, cp Checkpoint) error {
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.Dir, "checkpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.file(key))
}

// Delete implements CheckpointStore.
func (f FileCheckpoints) Delete(key string) error {
	err := os.Remove(f.file(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// UploadPhase names what UploadOptions.OnProgress is reporting.
type UploadPhase string

const (
	PhaseHashing   UploadPhase = "hashing"
	PhaseUploading UploadPhase = "uploading"
)

// UploadOptions configures one upload.
type UploadOptions struct {
	// Path is the client file.
	Path string
	// CheckpointKey identifies the transfer across runs (I172): server, tenant and
	// absolute path.
	CheckpointKey string
	Store         CheckpointStore
	// ChunkBytes asks for a chunk size; the server's record wins. Zero uses
	// api.DefaultChunkBytes.
	ChunkBytes int64
	// MaxAttempts is how many consecutive failures without progress are tolerated.
	MaxAttempts int
	// MinBackoff and MaxBackoff bound the delay between retries; 1 s and 30 s.
	MinBackoff, MaxBackoff time.Duration
	// OnProgress reports done and total bytes of the current phase.
	OnProgress func(phase UploadPhase, done, total int64)
	// OnRetry is told about each retry.
	OnRetry func(err error, delay time.Duration)
}

// UploadResult is a completed upload. Resumed says this run continued an earlier
// transfer; ResumedFrom is the offset it continued from.
type UploadResult struct {
	Upload      domain.Upload
	Resumed     bool
	ResumedFrom int64
	Checkpoint  Checkpoint
}

// Upload transfers a client file with checkpoints, per chunk checksums and retries
// (§5.4, I20, I172). The checkpoint stays in the store when it returns: the caller
// deletes it once whatever depends on the upload was accepted (I170).
func (c *Client) Upload(ctx context.Context, opts UploadOptions) (UploadResult, error) {
	if opts.Store == nil {
		return UploadResult{}, errs.New(errs.CodeInternal, "upload needs a checkpoint store")
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 8
	}
	if opts.MinBackoff == 0 {
		opts.MinBackoff = time.Second
	}
	if opts.MaxBackoff == 0 {
		opts.MaxBackoff = 30 * time.Second
	}

	file, err := os.Open(opts.Path)
	if err != nil {
		return UploadResult{}, errs.Wrap(errs.CodeInvalidArgument, err, "cannot open the file to upload")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return UploadResult{}, errs.Wrap(errs.CodeInvalidArgument, err, "cannot read the file to upload")
	}
	if !info.Mode().IsRegular() {
		return UploadResult{}, errs.Newf(errs.CodeInvalidArgument, "%s is not a regular file", filepath.Base(opts.Path))
	}
	if info.Size() == 0 {
		return UploadResult{}, errs.Newf(errs.CodeInvalidArgument, "%s is empty", filepath.Base(opts.Path))
	}

	cp, resumedCheckpoint, err := c.prepareCheckpoint(ctx, file, info, opts)
	if err != nil {
		return UploadResult{}, err
	}

	// One restart when the server no longer has a usable upload for this checkpoint.
	for restart := 0; ; restart++ {
		result, err := c.transfer(ctx, file, info, cp, resumedCheckpoint, opts)
		if !errors.Is(err, errUploadGone) {
			return result, err
		}
		if restart > 0 {
			return result, errs.New(errs.CodeFailedPrecondition, "the server refused the upload twice; start it again")
		}
		key, err := RandomKey()
		if err != nil {
			return result, err
		}
		cp.UploadID, cp.UploadULID, cp.IdempotencyKey = "", "", key
		resumedCheckpoint = false
		if err := opts.Store.Save(opts.CheckpointKey, cp); err != nil {
			return result, errs.Wrap(errs.CodeInternal, err, "cannot save the upload checkpoint")
		}
	}
}

var errUploadGone = errors.New("upload no longer usable")

// prepareCheckpoint reuses a checkpoint whose file is unchanged, or hashes the file
// and writes a new one before anything reaches the server (I170).
func (c *Client) prepareCheckpoint(ctx context.Context, file *os.File, info os.FileInfo, opts UploadOptions) (Checkpoint, bool, error) {
	existing, err := opts.Store.Load(opts.CheckpointKey)
	if err != nil {
		return Checkpoint{}, false, errs.Wrap(errs.CodeInternal, err, "cannot read the upload checkpoint")
	}
	if existing != nil && existing.SizeBytes == info.Size() && existing.ModTimeUnixNs == info.ModTime().UnixNano() && existing.SHA256 != "" {
		return *existing, true, nil
	}
	if existing != nil && existing.UploadID != "" {
		// The file changed under a stored upload. Best effort: the server also
		// expires abandoned uploads.
		_ = c.Do(ctx, Call{Route: api.RouteUploadsAbort, Path: PathParam("id", existing.UploadID)}, nil)
	}

	sum, err := hashFile(ctx, file, info.Size(), opts.OnProgress)
	if err != nil {
		return Checkpoint{}, false, err
	}
	key, err := RandomKey()
	if err != nil {
		return Checkpoint{}, false, err
	}
	cp := Checkpoint{
		Path:           opts.Path,
		SizeBytes:      info.Size(),
		ModTimeUnixNs:  info.ModTime().UnixNano(),
		SHA256:         sum,
		IdempotencyKey: key,
	}
	if err := opts.Store.Save(opts.CheckpointKey, cp); err != nil {
		return Checkpoint{}, false, errs.Wrap(errs.CodeInternal, err, "cannot save the upload checkpoint")
	}
	return cp, false, nil
}

func hashFile(ctx context.Context, file *os.File, size int64, progress func(UploadPhase, int64, int64)) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", errs.Wrap(errs.CodeInvalidArgument, err, "cannot read the file to upload")
	}
	hash := sha256.New()
	buf := make([]byte, 1<<20)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buf)
		if n > 0 {
			hash.Write(buf[:n])
			done += int64(n)
			if progress != nil {
				progress(PhaseHashing, done, size)
			}
		}
		if errors.Is(err, io.EOF) {
			return hex.EncodeToString(hash.Sum(nil)), nil
		}
		if err != nil {
			return "", errs.Wrap(errs.CodeInvalidArgument, err, "cannot read the file to upload")
		}
	}
}

func (c *Client) transfer(ctx context.Context, file *os.File, info os.FileInfo, cp Checkpoint, resumed bool, opts UploadOptions) (UploadResult, error) {
	result := UploadResult{Resumed: resumed, Checkpoint: cp}

	retry := newRetrier(c, opts)
	if cp.UploadID == "" {
		chunk := opts.ChunkBytes
		upload, err := withRetry(ctx, retry, func() (domain.Upload, error) {
			return Invoke[domain.Upload](ctx, c, Call{
				Route: api.RouteUploadsCreate,
				Body: api.CreateUploadRequest{
					Filename:   filepath.Base(opts.Path),
					SizeBytes:  info.Size(),
					SHA256:     cp.SHA256,
					ChunkBytes: chunk,
				},
				IdempotencyKey: cp.IdempotencyKey,
			})
		})
		if err != nil {
			return result, err
		}
		cp.UploadID, cp.UploadULID = string(upload.ID), string(upload.ULID)
		result.Checkpoint = cp
		if err := opts.Store.Save(opts.CheckpointKey, cp); err != nil {
			return result, errs.Wrap(errs.CodeInternal, err, "cannot save the upload checkpoint")
		}
	}

	upload, err := c.currentUpload(ctx, retry, cp.UploadID)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) || errs.IsCode(err, errs.CodeGone) {
			return result, errUploadGone
		}
		return result, err
	}
	if upload.SizeBytes != info.Size() || (upload.SHA256 != "" && upload.SHA256 != cp.SHA256) {
		return result, errUploadGone
	}
	switch upload.Status {
	case domain.UploadComplete:
		result.Upload = upload
		return result, nil
	case domain.UploadFailed, domain.UploadExpired:
		return result, errUploadGone
	}
	result.ResumedFrom = upload.ReceivedBytes
	result.Resumed = result.Resumed && upload.ReceivedBytes > 0

	chunkBytes := upload.ChunkBytes
	if chunkBytes <= 0 {
		chunkBytes = opts.ChunkBytes
	}
	if chunkBytes <= 0 {
		chunkBytes = api.DefaultChunkBytes
	}
	if chunkBytes > maxChunkBytes {
		chunkBytes = maxChunkBytes
	}
	buf := make([]byte, chunkBytes)

	offset := upload.ReceivedBytes
	if opts.OnProgress != nil {
		opts.OnProgress(PhaseUploading, offset, info.Size())
	}
	for {
		offset, err = c.sendChunks(ctx, file, cp.UploadID, offset, info.Size(), buf, retry, opts)
		if err != nil {
			return result, err
		}

		// The file must be what was hashed. complete verifies the digest, but a file
		// edited mid-transfer is better reported as what it is.
		if now, err := os.Stat(opts.Path); err == nil && (now.Size() != info.Size() || !now.ModTime().Equal(info.ModTime())) {
			return result, errs.New(errs.CodeFailedPrecondition, "the file changed during the upload; run the command again to upload the new content")
		}

		completed, err := withRetry(ctx, retry, func() (domain.Upload, error) {
			return Invoke[domain.Upload](ctx, c, Call{
				Route:          api.RouteUploadsComplete,
				Path:           PathParam("id", cp.UploadID),
				Body:           struct{}{},
				IdempotencyKey: DerivedKey("complete", cp.UploadULID, cp.UploadID),
				NoTimeout:      true, // the server hashes the whole file before answering
			})
		})
		if err == nil {
			result.Upload = completed
			return result, nil
		}
		if errs.IsCode(err, errs.CodeNotFound) || errs.IsCode(err, errs.CodeGone) {
			return result, errUploadGone
		}
		// The server's record can fall behind what it acknowledged after a crash
		// (I130). It says so with the offset, and the transfer continues from there.
		if lower, ok := reportedOffset(err); ok && lower < info.Size() {
			if spendErr := retry.spend(err); spendErr != nil {
				return result, spendErr
			}
			offset = lower
			continue
		}
		// A rejected digest can never succeed on this upload, so the next run must
		// start fresh rather than resume into the same rejection.
		if !IsTransient(err) && !errors.Is(err, context.Canceled) {
			_ = opts.Store.Delete(opts.CheckpointKey)
		}
		return result, err
	}
}

// sendChunks sends the file from offset to its end and returns the final offset.
// Only the server's own offset moves the transfer forward or back.
func (c *Client) sendChunks(ctx context.Context, file *os.File, uploadID string, offset, size int64, buf []byte, retry *retrier, opts UploadOptions) (int64, error) {
	report := func() {
		if opts.OnProgress != nil {
			opts.OnProgress(PhaseUploading, offset, size)
		}
	}
	for offset < size {
		if err := ctx.Err(); err != nil {
			return offset, err
		}
		n, err := file.ReadAt(buf, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return offset, errs.Wrap(errs.CodeInvalidArgument, err, "cannot read the file to upload")
		}
		if n == 0 {
			return offset, errs.New(errs.CodeInvalidArgument, "the file got shorter during the upload")
		}
		next, err := c.sendChunk(ctx, uploadID, offset, buf[:n])
		switch {
		case err == nil:
			if next <= offset || next > size {
				// A wrong answer is resolved by asking, never by assuming.
				current, getErr := c.currentUpload(ctx, retry, uploadID)
				if getErr != nil {
					return offset, getErr
				}
				next = current.ReceivedBytes
			}
			if next > offset {
				retry.progress()
			} else if waitErr := retry.wait(ctx, errs.New(errs.CodeUnavailable, "the server makes no progress on this upload")); waitErr != nil {
				return offset, waitErr
			}
			offset = next
			report()
		case errs.IsCode(err, errs.CodeConflict) || errs.IsCode(err, errs.CodeFailedPrecondition):
			// A wrong offset answers 409 with the current one (I20). Re-read it. Moving
			// to a new offset needs no delay, but it still spends budget, so a server
			// that keeps refusing cannot loop this forever.
			current, getErr := c.currentUpload(ctx, retry, uploadID)
			if getErr != nil {
				return offset, getErr
			}
			if current.Status == domain.UploadFailed || current.Status == domain.UploadExpired {
				return offset, errUploadGone
			}
			if current.ReceivedBytes == offset {
				if waitErr := retry.wait(ctx, err); waitErr != nil {
					return offset, waitErr
				}
			} else if spendErr := retry.spend(err); spendErr != nil {
				return offset, spendErr
			}
			offset = current.ReceivedBytes
			report()
		case errs.IsCode(err, errs.CodeNotFound) || errs.IsCode(err, errs.CodeGone):
			return offset, errUploadGone
		case IsTransient(err) || errs.IsCode(err, errs.CodeValidationFailed):
			// A rejected chunk digest means the bytes changed in transit. The server
			// truncated back (I130), so it resends like any other failure.
			if waitErr := retry.wait(ctx, err); waitErr != nil {
				return offset, waitErr
			}
			current, getErr := c.currentUpload(ctx, retry, uploadID)
			if getErr != nil {
				return offset, getErr
			}
			offset = current.ReceivedBytes
			report()
		default:
			return offset, err
		}
	}
	return offset, nil
}

// reportedOffset reads details.offset from a refused complete (I130).
func reportedOffset(err error) (int64, bool) {
	var coded *errs.Error
	if !errors.As(err, &coded) || coded.Code != errs.CodeFailedPrecondition {
		return 0, false
	}
	value, ok := coded.Details["offset"].(float64)
	return int64(value), ok
}

// DerivedKey builds an idempotency key that is stable across retries of a command.
func DerivedKey(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))[:40]
}

func (c *Client) currentUpload(ctx context.Context, retry *retrier, uploadID string) (domain.Upload, error) {
	return withRetry(ctx, retry, func() (domain.Upload, error) {
		return Invoke[domain.Upload](ctx, c, Call{Route: api.RouteUploadsGet, Path: PathParam("id", uploadID)})
	})
}

// sendChunk PATCHes one chunk and returns the new offset, or 0 when the answer did
// not say; the caller then asks with GET (I171).
func (c *Client) sendChunk(ctx context.Context, uploadID string, offset int64, chunk []byte) (int64, error) {
	sum := sha256.Sum256(chunk)
	req, _, err := c.request(ctx, Call{Route: api.RouteUploadsChunk, Path: PathParam("id", uploadID)}, bytes.NewReader(chunk), "application/offset+octet-stream")
	if err != nil {
		return 0, err
	}
	req.ContentLength = int64(len(chunk))
	req.Header.Set(api.HeaderUploadOffset, strconv.FormatInt(offset, 10))
	req.Header.Set(api.HeaderUploadChecksum, "sha256 "+base64.StdEncoding.EncodeToString(sum[:]))

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, c.transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := decodeError(resp)
		var coded *errs.Error
		if errors.As(err, &coded) {
			if delay := parseRetryAfter(resp); delay != nil {
				return 0, coded.With("retry_after_seconds", delay.Seconds())
			}
		}
		return 0, err
	}
	if value := resp.Header.Get(api.HeaderUploadOffset); value != "" {
		if next, err := strconv.ParseInt(value, 10, 64); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
			return next, nil
		}
	}
	var body struct {
		ReceivedBytes *int64 `json:"received_bytes"`
		Offset        *int64 `json:"offset"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if json.Unmarshal(raw, &body) == nil {
		if body.ReceivedBytes != nil {
			return *body.ReceivedBytes, nil
		}
		if body.Offset != nil {
			return *body.Offset, nil
		}
	}
	return 0, nil
}

// retrier is the retry budget: consecutive failures without progress.
type retrier struct {
	client *Client
	opts   UploadOptions
	count  int
}

func newRetrier(c *Client, opts UploadOptions) *retrier {
	return &retrier{client: c, opts: opts}
}

func (r *retrier) progress() { r.count = 0 }

// wait spends one attempt and sleeps, or returns the last error when the budget ran
// out. Backoff is exponential with full jitter on its upper half.
func (r *retrier) wait(ctx context.Context, cause error) error {
	if err := r.spend(cause); err != nil {
		return err
	}
	delay := r.opts.MinBackoff << (r.count - 1)
	if delay <= 0 || delay > r.opts.MaxBackoff {
		delay = r.opts.MaxBackoff
	}
	delay = delay/2 + jitter(delay/2)
	var coded *errs.Error
	if errors.As(cause, &coded) {
		if secs, ok := coded.Details["retry_after_seconds"].(float64); ok {
			delay = time.Duration(secs * float64(time.Second))
		}
	}
	if r.opts.OnRetry != nil {
		r.opts.OnRetry(cause, delay)
	}
	return r.client.sleep(ctx, delay)
}

// spend uses one attempt of the budget, or reports that the budget ran out.
func (r *retrier) spend(cause error) error {
	r.count++
	if r.count < r.opts.MaxAttempts {
		return nil
	}
	message := fmt.Sprintf("giving up after %d attempts without progress; run the command again to resume", r.count)
	var coded *errs.Error
	if errors.As(cause, &coded) {
		out := *coded
		out.Message = message + ": " + coded.Message
		return &out
	}
	return errs.Wrap(errs.CodeUnavailable, cause, message)
}

func jitter(limit time.Duration) time.Duration {
	if limit <= 0 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return limit / 2
	}
	return time.Duration(binary.BigEndian.Uint64(b[:]) % uint64(limit))
}

func withRetry[T any](ctx context.Context, r *retrier, f func() (T, error)) (T, error) {
	for {
		out, err := f()
		if err == nil {
			// Only an accepted chunk resets the budget: a server that answers reads
			// and refuses every chunk is not making progress.
			return out, nil
		}
		if !IsTransient(err) {
			return out, err
		}
		if waitErr := r.wait(ctx, err); waitErr != nil {
			return out, waitErr
		}
	}
}
