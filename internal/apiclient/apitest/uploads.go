package apitest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
)

// Uploads is a fake of the upload protocol of I20, with failure injection. It
// verifies chunk checksums, offsets and the final digest as the real server must.
type Uploads struct {
	mu        sync.Mutex
	next      uint64
	byID      map[string]*fakeUpload
	byKey     map[string]string
	chunk     int64
	received  int64 // bytes accepted across all uploads, duplicates would inflate it
	chunks    int
	failNext  []func(w http.ResponseWriter, r *http.Request) bool
	completes int
	creates   int
	OnChunk   func(received int64) // called after each accepted chunk
}

type fakeUpload struct {
	record domain.Upload
	data   []byte
}

// EnableUploads installs the upload handlers with a chunk size.
func (s *Server) EnableUploads(chunkBytes int64) *Uploads {
	u := &Uploads{byID: map[string]*fakeUpload{}, byKey: map[string]string{}, chunk: chunkBytes}
	s.Handle(api.RouteUploadsCreate, u.create)
	s.Handle(api.RouteUploadsGet, u.get)
	s.Handle(api.RouteUploadsChunk, u.patch)
	s.Handle(api.RouteUploadsComplete, u.complete)
	s.Handle(api.RouteUploadsAbort, u.abort)
	return u
}

// FailNextChunk makes the next chunk request run f; f returns true when it wrote an
// answer of its own. Calls queue.
func (u *Uploads) FailNextChunk(f func(w http.ResponseWriter, r *http.Request) bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.failNext = append(u.failNext, f)
}

// Status answers the next chunk with a coded error, without consuming its body.
func Status(code errs.Code) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, _ *http.Request) bool {
		WriteError(w, errs.New(code, "injected failure"))
		return true
	}
}

// DropConnection closes the connection with no answer.
func DropConnection() func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, _ *http.Request) bool {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return true
	}
}

// PartialWrite accepts the first n bytes of the chunk and then drops the connection,
// as a server that had received part of it when the network failed.
func (u *Uploads) PartialWrite(n int) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		buf := make([]byte, n)
		read, _ := io.ReadFull(r.Body, buf)
		ref := r.PathValue("id")
		u.mu.Lock()
		if up := u.byID[ref]; up != nil {
			up.data = append(up.data, buf[:read]...)
			up.record.ReceivedBytes = int64(len(up.data))
		}
		u.mu.Unlock()
		return DropConnection()(w, r)
	}
}

// Expire marks an upload expired, as the server's retention would.
func (u *Uploads) Expire(ref string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if up := u.byID[ref]; up != nil {
		up.record.Status = domain.UploadExpired
	}
}

// Truncate drops received bytes, as the server's crash recovery can (I130).
func (u *Uploads) Truncate(ref string, size int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if up := u.byID[ref]; up != nil && int64(len(up.data)) > size {
		up.data = up.data[:size]
		up.record.ReceivedBytes = size
	}
}

// Data returns the bytes received by an upload.
func (u *Uploads) Data(ref string) []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	if up := u.byID[ref]; up != nil {
		return append([]byte(nil), up.data...)
	}
	return nil
}

// Record returns an upload's record.
func (u *Uploads) Record(ref string) (domain.Upload, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if up := u.byID[ref]; up != nil {
		return up.record, true
	}
	return domain.Upload{}, false
}

// TotalAccepted is every byte ever accepted; more than a file's size means bytes
// were sent twice.
func (u *Uploads) TotalAccepted() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.received
}

// Created is how many distinct uploads were created.
func (u *Uploads) Created() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.creates
}

// Completed is how many complete calls succeeded.
func (u *Uploads) Completed() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.completes
}

func (u *Uploads) create(w http.ResponseWriter, r *http.Request) {
	var req api.CreateUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, errs.New(errs.CodeInvalidArgument, "bad body"))
		return
	}
	if req.SizeBytes <= 0 || len(req.SHA256) != 64 || req.Filename == "" || strings.ContainsAny(req.Filename, "/\\") {
		WriteError(w, errs.New(errs.CodeInvalidArgument, "bad upload request"))
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	key := r.Header.Get(api.HeaderIdempotencyKey)
	if existing, ok := u.byKey[key]; ok {
		WriteJSON(w, http.StatusOK, u.byID[existing].record)
		return
	}
	u.next++
	u.creates++
	friendly := id.Format(id.PrefixUpload, u.next)
	up := &fakeUpload{record: domain.Upload{
		ID: friendly, ULID: id.New(), TenantID: domain.TenantID(r.Header.Get(api.HeaderTenant)),
		Filename: req.Filename, SizeBytes: req.SizeBytes, SHA256: req.SHA256, ChunkBytes: u.chunk,
		Status: domain.UploadPending, CreatedAt: time.Now().UTC(),
	}}
	u.byID[string(friendly)] = up
	u.byKey[key] = string(friendly)
	WriteJSON(w, http.StatusCreated, up.record)
}

func (u *Uploads) get(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	up := u.byID[r.PathValue("id")]
	if up == nil {
		WriteError(w, errs.NotFound("upload"))
		return
	}
	WriteJSON(w, http.StatusOK, up.record)
}

func (u *Uploads) patch(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	var inject func(http.ResponseWriter, *http.Request) bool
	if len(u.failNext) > 0 {
		inject = u.failNext[0]
		u.failNext = u.failNext[1:]
	}
	u.mu.Unlock()
	if inject != nil && inject(w, r) {
		return
	}

	offset, err := strconv.ParseInt(r.Header.Get(api.HeaderUploadOffset), 10, 64)
	if err != nil {
		WriteError(w, errs.New(errs.CodeInvalidArgument, "missing Upload-Offset"))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	up := u.byID[r.PathValue("id")]
	if up == nil {
		WriteError(w, errs.NotFound("upload"))
		return
	}
	if offset != up.record.ReceivedBytes {
		w.Header().Set(api.HeaderUploadOffset, strconv.FormatInt(up.record.ReceivedBytes, 10))
		WriteError(w, errs.Newf(errs.CodeConflict, "wrong offset, the current one is %d", up.record.ReceivedBytes).With("offset", up.record.ReceivedBytes))
		return
	}
	if want := r.Header.Get(api.HeaderUploadChecksum); want != "" {
		sum := sha256.Sum256(body)
		if want != "sha256 "+base64.StdEncoding.EncodeToString(sum[:]) {
			WriteError(w, errs.New(errs.CodeValidationFailed, "chunk checksum mismatch"))
			return
		}
	}
	up.data = append(up.data, body...)
	up.record.ReceivedBytes = int64(len(up.data))
	u.received += int64(len(body))
	u.chunks++
	w.Header().Set(api.HeaderUploadOffset, strconv.FormatInt(up.record.ReceivedBytes, 10))
	w.WriteHeader(http.StatusNoContent)
	if u.OnChunk != nil {
		u.OnChunk(up.record.ReceivedBytes)
	}
}

func (u *Uploads) complete(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	up := u.byID[r.PathValue("id")]
	if up == nil {
		WriteError(w, errs.NotFound("upload"))
		return
	}
	sum := sha256.Sum256(up.data)
	if int64(len(up.data)) != up.record.SizeBytes || hex.EncodeToString(sum[:]) != up.record.SHA256 {
		up.record.Status = domain.UploadFailed
		WriteError(w, errs.New(errs.CodeValidationFailed, "size or SHA-256 mismatch"))
		return
	}
	up.record.Status = domain.UploadComplete
	u.completes++
	WriteJSON(w, http.StatusOK, up.record)
}

func (u *Uploads) abort(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if up := u.byID[r.PathValue("id")]; up != nil {
		up.record.Status = domain.UploadFailed
	}
	w.WriteHeader(http.StatusNoContent)
}
