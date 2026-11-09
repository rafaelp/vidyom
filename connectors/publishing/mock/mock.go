// Package mock is the mock publishing destination (I30, I234): driver mock, used by
// tests and the demo instead of a real platform. It is a test double and must stay
// out of default configuration. It stores no video bytes, only their hash, in a
// JSON file that survives restarts, so idempotency and reconciliation can be proven
// across a killed daemon.
//
// Parameters:
//
//	{
//	  "stateDir": "/var/lib/vidyom/mock-destination", // required
//	  "processingSeconds": 0,   // time a video stays in processing
//	  "reconcile": true,        // false answers capability_unsupported
//	  "inject": {               // each counter fires that many times, once ever
//	    "fail_upload": 0,          // upload breaks midway, retriable, resumes later
//	    "outcome_unknown": 0,      // upload stored, answer lost
//	    "outcome_unknown_lost": 0, // upload not stored, answer lost
//	    "reject": 0,               // upload stored, refused by the platform (D52)
//	    "fail_remove": 0           // delete or set-private fails, retriable
//	  }
//	}
package mock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Driver is the name used in publishing.json.
const Driver = "mock"

// Version is the driver version.
const Version = "1.0.0"

// BaseURL is where mock videos claim to live. The host is fictional.
const BaseURL = "https://media.example.internal"

// Injection names, the keys of parameters.inject.
const (
	InjectFailUpload         = "fail_upload"
	InjectOutcomeUnknown     = "outcome_unknown"
	InjectOutcomeUnknownLost = "outcome_unknown_lost"
	InjectReject             = "reject"
	InjectFailRemove         = "fail_remove"
)

const chunkBytes = 1 << 20

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Register declares the driver.
func Register(r connector.Registrar) {
	r.RegisterPublishing(info(true), open)
}

func info(reconcile bool) connector.Info {
	return connector.Info{
		Family:    connector.FamilyPublishing,
		Driver:    Driver,
		Version:   Version,
		Available: true,
		Reason:    "test destination with persisted state and failure injection, never for production (I30)",
		Capabilities: map[string]bool{
			"resumable_upload": true, "thumbnails": true, "captions": true, "scheduling": true,
			"update": true, "delete": true, "reconcile": reconcile,
		},
	}
}

type parameters struct {
	StateDir          string         `json:"stateDir"`
	ProcessingSeconds float64        `json:"processingSeconds"`
	Reconcile         *bool          `json:"reconcile"`
	Inject            map[string]int `json:"inject"`
}

func open(_ context.Context, p connector.PublishingParams) (connector.Publishing, error) {
	var params parameters
	if len(p.Config.Parameters) > 0 {
		if err := json.Unmarshal(p.Config.Parameters, &params); err != nil {
			return nil, errs.Wrap(errs.CodeInvalidArgument, err, "invalid mock destination parameters")
		}
	}
	if params.StateDir == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "mock destination needs parameters.stateDir: its state must survive restarts")
	}
	if err := p.Tenant.Validate(); err != nil {
		return nil, errs.Wrap(errs.CodeInvalidArgument, err, "invalid tenant")
	}
	if !safeName.MatchString(p.Destination) {
		return nil, errs.Newf(errs.CodeInvalidArgument, "invalid destination name %q", p.Destination)
	}
	for name := range params.Inject {
		switch name {
		case InjectFailUpload, InjectOutcomeUnknown, InjectOutcomeUnknownLost, InjectReject, InjectFailRemove:
		default:
			return nil, errs.Newf(errs.CodeInvalidArgument, "unknown mock injection %q", name)
		}
	}
	return &Destination{
		path:       StatePath(params.StateDir, p.Tenant, p.Destination),
		processing: time.Duration(params.ProcessingSeconds * float64(time.Second)),
		reconcile:  params.Reconcile == nil || *params.Reconcile,
		inject:     params.Inject,
		now:        time.Now,
	}, nil
}

// StatePath is where a destination keeps its state.
func StatePath(stateDir string, tenant domain.TenantID, destination string) string {
	return filepath.Join(stateDir, string(tenant), destination+".json")
}

// State is the persisted destination, readable by tests as evidence.
type State struct {
	Videos map[string]*Video `json:"videos"`
	// ByKey maps an idempotency key to its remote ID.
	ByKey map[string]string `json:"by_key"`
	// Partial is bytes received per key for an upload that has not finished.
	Partial map[string]int64 `json:"partial,omitempty"`
	// Uploads counts uploads that created a video; a repeat never increments it.
	Uploads int `json:"uploads"`
	// Repeats counts Upload calls answered from an existing video.
	Repeats int `json:"repeats"`
	// Resumes counts uploads continued from a received offset.
	Resumes int `json:"resumes"`
	// Injected counts injections that have already fired.
	Injected map[string]int `json:"injected,omitempty"`
}

// Caption is a stored caption track. Tests scan it for what reached the destination.
type Caption struct {
	Format string `json:"format"`
	Text   string `json:"text"`
}

// Video is one video at the destination.
type Video struct {
	RemoteID        string                  `json:"remote_id"`
	IdempotencyKey  string                  `json:"idempotency_key"`
	SHA256          string                  `json:"sha256"`
	SizeBytes       int64                   `json:"size_bytes"`
	Metadata        connector.VideoMetadata `json:"metadata"`
	Visibility      domain.Visibility       `json:"visibility"`
	CreatedAt       time.Time               `json:"created_at"`
	ReadyAt         time.Time               `json:"ready_at"`
	Rejected        string                  `json:"rejected,omitempty"`
	Deleted         bool                    `json:"deleted,omitempty"`
	ThumbnailSHA256 string                  `json:"thumbnail_sha256,omitempty"`
	Captions        map[string]Caption      `json:"captions,omitempty"`
}

// ReadState reads a destination's file, for evidence in tests and the demo.
func ReadState(stateDir string, tenant domain.TenantID, destination string) (State, error) {
	return loadState(StatePath(stateDir, tenant, destination))
}

// Destination is the connector.
type Destination struct {
	path       string
	processing time.Duration
	reconcile  bool
	inject     map[string]int
	now        func() time.Time
}

var (
	_ connector.Publishing   = (*Destination)(nil)
	_ connector.PublicLinker = (*Destination)(nil)
)

// One mutex per state file. Several connector instances can share a file, and the
// daemon is the only process that opens it.
var fileLocks sync.Map

func (d *Destination) lock() func() {
	m, _ := fileLocks.LoadOrStore(d.path, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func loadState(path string) (State, error) {
	s := State{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s, errs.Wrap(errs.CodeUnavailable, err, "cannot read the mock destination's state")
	}
	if err == nil {
		if err := json.Unmarshal(data, &s); err != nil {
			return s, errs.Wrap(errs.CodeInternal, err, "the mock destination's state is corrupt")
		}
	}
	if s.Videos == nil {
		s.Videos = map[string]*Video{}
	}
	if s.ByKey == nil {
		s.ByKey = map[string]string{}
	}
	if s.Partial == nil {
		s.Partial = map[string]int64{}
	}
	if s.Injected == nil {
		s.Injected = map[string]int{}
	}
	return s, nil
}

func (d *Destination) save(s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode mock state: %w", err)
	}
	dir := filepath.Dir(d.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot create the mock destination's state directory")
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(d.path)+".tmp-*")
	if err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot write the mock destination's state")
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), d.path)
	}
	if err == nil {
		var dirFile *os.File
		if dirFile, err = os.Open(dir); err == nil {
			err = dirFile.Sync()
			dirFile.Close()
		}
	}
	if err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot write the mock destination's state")
	}
	return nil
}

// update runs fn with the current state under the file lock and saves the result.
func (d *Destination) update(fn func(*State) error) error {
	defer d.lock()()
	s, err := loadState(d.path)
	if err != nil {
		return err
	}
	if err := fn(&s); err != nil {
		return err
	}
	return d.save(s)
}

// fire consumes one injection when any is left. Its counter is in the file, so an
// injection does not fire again after a restart.
func (d *Destination) fire(s *State, name string) bool {
	if s.Injected[name] >= d.inject[name] {
		return false
	}
	s.Injected[name]++
	return true
}

// RemoteID derives the deterministic remote ID of an idempotency key (I30).
func RemoteID(idempotencyKey string) string {
	sum := sha256.Sum256([]byte("mock\x00" + idempotencyKey))
	return "mock-" + hex.EncodeToString(sum[:8])
}

func (d *Destination) view(v *Video) connector.RemoteVideo {
	out := connector.RemoteVideo{RemoteID: v.RemoteID, Visibility: v.Visibility}
	now := d.now()
	switch {
	case v.Deleted:
		out.Status = connector.RemoteRemoved
		out.Visibility = domain.VisibilityPrivate
	case v.Rejected != "":
		out.Status = connector.RemoteRejected
		out.Reason = v.Rejected
	case now.Before(v.ReadyAt):
		out.Status = connector.RemoteProcessing
	case v.Metadata.ScheduledAt != nil && now.Before(*v.Metadata.ScheduledAt):
		out.Status = connector.RemoteScheduled
		out.URL = videoURL(v.RemoteID)
	default:
		out.Status = connector.RemoteReady
		out.URL = videoURL(v.RemoteID)
		if v.Metadata.ScheduledAt != nil {
			out.Visibility = domain.VisibilityPublic
		}
	}
	return out
}

func videoURL(remoteID string) string { return BaseURL + "/watch/" + remoteID }

// Info implements connector.Publishing.
func (d *Destination) Info() connector.Info { return info(d.reconcile) }

// Capabilities implements connector.Publishing.
func (d *Destination) Capabilities() connector.PublishingCapabilities {
	return connector.PublishingCapabilities{
		ResumableUpload: true,
		Thumbnails:      true,
		Captions:        true,
		Scheduling:      true,
		Update:          true,
		Delete:          true,
		Reconcile:       d.reconcile,
		Visibilities:    []domain.Visibility{domain.VisibilityPrivate, domain.VisibilityUnlisted, domain.VisibilityPublic},
	}
}

// Upload stores a video once per idempotency key. It reads the whole file, so size
// and SHA-256 are verified as a real destination's processing would notice.
func (d *Destination) Upload(ctx context.Context, req connector.UploadRequest) (connector.RemoteVideo, error) {
	if req.IdempotencyKey == "" {
		return connector.RemoteVideo{}, errs.New(errs.CodeInvalidArgument, "an upload needs an idempotency key")
	}
	if req.File == nil {
		return connector.RemoteVideo{}, errs.New(errs.CodeInvalidArgument, "an upload needs a file")
	}

	var existing *connector.RemoteVideo
	var offset int64
	var breakAt int64 = -1
	err := d.update(func(s *State) error {
		if remoteID, ok := s.ByKey[req.IdempotencyKey]; ok {
			s.Repeats++
			v := d.view(s.Videos[remoteID])
			existing = &v
			return nil
		}
		offset = s.Partial[req.IdempotencyKey]
		if offset > 0 {
			s.Resumes++
		}
		if d.fire(s, InjectFailUpload) {
			breakAt = offset + (req.SizeBytes-offset)/2
			s.Partial[req.IdempotencyKey] = breakAt
		}
		return nil
	})
	if err != nil {
		return connector.RemoteVideo{}, err
	}
	if existing != nil {
		return *existing, nil
	}
	if breakAt >= 0 {
		return connector.RemoteVideo{}, errs.New(errs.CodeUnavailable, "the mock destination broke the upload midway (injected)")
	}

	sum, size, err := d.receive(ctx, req, offset)
	if err != nil {
		return connector.RemoteVideo{}, err
	}
	if req.SizeBytes > 0 && size != req.SizeBytes {
		return connector.RemoteVideo{}, errs.Newf(errs.CodeInvalidArgument, "the mock destination received %d bytes, expected %d", size, req.SizeBytes)
	}
	if req.SHA256 != "" && sum != req.SHA256 {
		return connector.RemoteVideo{}, errs.New(errs.CodeInvalidArgument, "the mock destination received a different file than declared")
	}

	var out connector.RemoteVideo
	var answerLost bool
	err = d.update(func(s *State) error {
		if remoteID, ok := s.ByKey[req.IdempotencyKey]; ok {
			s.Repeats++
			out = d.view(s.Videos[remoteID])
			return nil
		}
		if d.fire(s, InjectOutcomeUnknownLost) {
			answerLost = true
			return nil
		}
		now := d.now()
		v := &Video{
			RemoteID:       RemoteID(req.IdempotencyKey),
			IdempotencyKey: req.IdempotencyKey,
			SHA256:         sum,
			SizeBytes:      size,
			Metadata:       req.Metadata,
			Visibility:     req.Metadata.Visibility,
			CreatedAt:      now,
			ReadyAt:        now.Add(d.processing),
		}
		if v.Visibility == "" {
			v.Visibility = domain.VisibilityPrivate
		}
		if d.fire(s, InjectReject) {
			v.Rejected = "refused by the mock destination's policy (injected)"
		}
		answerLost = d.fire(s, InjectOutcomeUnknown)
		s.Videos[v.RemoteID] = v
		s.ByKey[req.IdempotencyKey] = v.RemoteID
		delete(s.Partial, req.IdempotencyKey)
		s.Uploads++
		out = d.view(v)
		return nil
	})
	if err != nil {
		return connector.RemoteVideo{}, err
	}
	if answerLost {
		return connector.RemoteVideo{}, connector.OutcomeUnknown(errs.New(errs.CodeUnavailable, "the connection to the mock destination was lost before its answer (injected)"))
	}
	return out, nil
}

// receive hashes the file, reporting progress only for bytes past the resume offset.
func (d *Destination) receive(ctx context.Context, req connector.UploadRequest, offset int64) (string, int64, error) {
	h := sha256.New()
	buf := make([]byte, chunkBytes)
	var pos int64
	for {
		if err := ctx.Err(); err != nil {
			saveErr := d.update(func(s *State) error {
				if pos > s.Partial[req.IdempotencyKey] {
					s.Partial[req.IdempotencyKey] = pos
				}
				return nil
			})
			return "", 0, errors.Join(err, saveErr)
		}
		n, err := req.File.ReadAt(buf, pos)
		if n > 0 {
			h.Write(buf[:n])
			pos += int64(n)
			if req.Progress != nil && pos > offset {
				req.Progress(pos)
			}
		}
		if errors.Is(err, io.EOF) {
			return hex.EncodeToString(h.Sum(nil)), pos, nil
		}
		if err != nil {
			return "", 0, errs.Wrap(errs.CodeUnavailable, err, "cannot read the file to upload")
		}
	}
}

func (d *Destination) read(remoteID string, fn func(connector.RemoteVideo)) error {
	defer d.lock()()
	s, err := loadState(d.path)
	if err != nil {
		return err
	}
	v, ok := s.Videos[remoteID]
	if !ok {
		return errs.NotFound("video")
	}
	fn(d.view(v))
	return nil
}

// Get implements connector.Publishing.
func (d *Destination) Get(_ context.Context, remoteID string) (connector.RemoteVideo, error) {
	var out connector.RemoteVideo
	err := d.read(remoteID, func(v connector.RemoteVideo) { out = v })
	return out, err
}

// GetStatus implements connector.Publishing.
func (d *Destination) GetStatus(ctx context.Context, remoteID string) (connector.RemoteVideo, error) {
	return d.Get(ctx, remoteID)
}

// GetURL returns a URL only once the destination has one (R43).
func (d *Destination) GetURL(ctx context.Context, remoteID string) (string, error) {
	v, err := d.Get(ctx, remoteID)
	if err != nil {
		return "", err
	}
	if v.URL == "" {
		return "", errs.New(errs.CodeFailedPrecondition, "this video has no URL yet")
	}
	return v.URL, nil
}

func (d *Destination) change(remoteID string, fn func(s *State, v *Video) error) (connector.RemoteVideo, error) {
	var out connector.RemoteVideo
	err := d.update(func(s *State) error {
		v, ok := s.Videos[remoteID]
		if !ok || v.Deleted {
			return errs.NotFound("video")
		}
		if err := fn(s, v); err != nil {
			return err
		}
		out = d.view(v)
		return nil
	})
	return out, err
}

// Update implements connector.Publishing.
func (d *Destination) Update(_ context.Context, remoteID string, meta connector.VideoMetadata) (connector.RemoteVideo, error) {
	return d.change(remoteID, func(_ *State, v *Video) error {
		v.Metadata = meta
		if meta.Visibility != "" {
			v.Visibility = meta.Visibility
		}
		return nil
	})
}

// Delete removes a video. Deleting a deleted video is a no-op; an unknown ID is
// errs.CodeNotFound, which callers treat as already removed.
func (d *Destination) Delete(_ context.Context, remoteID string) error {
	var failed error
	err := d.update(func(s *State) error {
		v, ok := s.Videos[remoteID]
		if !ok {
			return errs.NotFound("video")
		}
		if v.Deleted {
			return nil
		}
		if d.fire(s, InjectFailRemove) {
			failed = errs.New(errs.CodeUnavailable, "the mock destination refused the removal (injected)")
			return nil
		}
		v.Deleted = true
		return nil
	})
	if err != nil {
		return err
	}
	return failed
}

// SetVisibility implements connector.Publishing.
func (d *Destination) SetVisibility(_ context.Context, remoteID string, vis domain.Visibility) (connector.RemoteVideo, error) {
	var failed error
	out, err := d.change(remoteID, func(s *State, v *Video) error {
		if vis == domain.VisibilityPrivate && v.Visibility != vis && d.fire(s, InjectFailRemove) {
			failed = errs.New(errs.CodeUnavailable, "the mock destination refused the change (injected)")
			return nil
		}
		v.Visibility = vis
		return nil
	})
	if err != nil {
		return connector.RemoteVideo{}, err
	}
	if failed != nil {
		return connector.RemoteVideo{}, failed
	}
	return out, nil
}

// UploadThumbnail records the image's hash.
func (d *Destination) UploadThumbnail(_ context.Context, remoteID string, image io.Reader, _ string) error {
	data, err := io.ReadAll(image)
	if err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot read the thumbnail")
	}
	sum := sha256.Sum256(data)
	_, err = d.change(remoteID, func(_ *State, v *Video) error {
		v.ThumbnailSHA256 = hex.EncodeToString(sum[:])
		return nil
	})
	return err
}

// UploadCaptions stores the track's text, replacing the language's previous one.
func (d *Destination) UploadCaptions(_ context.Context, remoteID, language, format string, captions io.Reader) error {
	data, err := io.ReadAll(captions)
	if err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot read the captions")
	}
	_, err = d.change(remoteID, func(_ *State, v *Video) error {
		if v.Captions == nil {
			v.Captions = map[string]Caption{}
		}
		v.Captions[language] = Caption{Format: format, Text: string(data)}
		return nil
	})
	return err
}

// Reconcile implements connector.Publishing.
func (d *Destination) Reconcile(_ context.Context, idempotencyKey string) (*connector.RemoteVideo, error) {
	if !d.reconcile {
		return nil, errs.New(errs.CodeCapabilityUnsupported, "this mock destination is configured without reconcile")
	}
	defer d.lock()()
	s, err := loadState(d.path)
	if err != nil {
		return nil, err
	}
	remoteID, ok := s.ByKey[idempotencyKey]
	if !ok {
		return nil, nil
	}
	v := d.view(s.Videos[remoteID])
	return &v, nil
}

// PublicLinks implements connector.PublicLinker. The mock has no thumbnails.
func (d *Destination) PublicLinks(remote connector.RemoteVideo) connector.PublicLinks {
	if remote.RemoteID == "" {
		return connector.PublicLinks{}
	}
	return connector.PublicLinks{EmbedURL: BaseURL + "/embed/" + remote.RemoteID, SupportsTimestamps: true}
}
