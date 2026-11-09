package storagenodes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/workers/nodeutil"
	"github.com/rafaelp/vidyom/internal/workers/provenance"
)

// Register adds every storage.* node.
func Register(r node.Registrar) {
	r.RegisterNode(putNode{nodeutil.NewBase("storage.put")})
	r.RegisterNode(getNode{nodeutil.NewBase("storage.get")})
	r.RegisterNode(verifyNode{nodeutil.NewBase("storage.verify")})
	r.RegisterNode(deleteNode{nodeutil.NewBase("storage.delete")})
	r.RegisterNode(copyNode{nodeutil.NewBase("storage.copy")})
}

// storedObject is the catalog's storedObject: domain.MediaFile plus uri.
type storedObject struct {
	URI       string             `json:"uri"`
	Storage   domain.StorageRole `json:"storage"`
	Key       string             `json:"key"`
	SHA256    string             `json:"sha256,omitempty"`
	SizeBytes int64              `json:"size_bytes"`
	Verified  bool               `json:"verified"`
	Profile   string             `json:"profile,omitempty"`
	Location  domain.Location    `json:"location"`
}

func stored(role domain.StorageRole, info connector.ObjectInfo, verified bool, profile string) storedObject {
	return storedObject{
		URI:       domain.InputURI{Kind: domain.URIStorage, Role: role, Key: info.Key}.String(),
		Storage:   role,
		Key:       info.Key,
		SHA256:    info.SHA256,
		SizeBytes: info.SizeBytes,
		Verified:  verified,
		Profile:   profile,
		Location:  info.Location,
	}
}

func parseRole(role domain.StorageRole) error {
	if !role.Valid() {
		return errs.Newf(errs.CodeInvalidArgument, "unknown storage role %q", role)
	}
	return nil
}

func parseKey(key string) error {
	if err := domain.ValidateStorageKey(key); err != nil {
		return errs.Wrap(errs.CodeInvalidArgument, err, "invalid storage key")
	}
	return nil
}

func parseStorageSource(source string) (domain.InputURI, error) {
	uri, err := domain.ParseInputURI(source)
	if err != nil {
		return domain.InputURI{}, errs.Wrap(errs.CodeInvalidArgument, err, "invalid source reference")
	}
	if uri.Kind != domain.URIStorage {
		return domain.InputURI{}, errs.New(errs.CodeInvalidArgument, "this node needs a storage:// reference")
	}
	return uri, nil
}

// archiveAllowed applies §3.2: reading archive is an explicit, permitted act.
func archiveAllowed(env node.Env, role domain.StorageRole, allowArchive bool) error {
	if role != domain.RoleArchive {
		return nil
	}
	if !allowArchive {
		return errs.New(errs.CodeForbiddenStorageRole, "reading from the archive role is forbidden here")
	}
	if !env.HasPermission(domain.PermStorageArchiveRestore) {
		return errs.Forbidden(string(domain.PermStorageArchiveRestore))
	}
	return nil
}

func progressFor(env node.Env, size int64, message string) func(int64) {
	if size <= 0 {
		return nil
	}
	return func(done int64) {
		env.Progress(float64(done)/float64(size)*100, message)
	}
}

// ---- storage.put ----

type putNode struct{ nodeutil.Base }

type putParams struct {
	Storage        domain.StorageRole `json:"storage"`
	Source         string             `json:"source"`
	Key            string             `json:"key"`
	VerifyChecksum bool               `json:"verifyChecksum"`
	ContentType    string             `json:"contentType"`
	StorageClass   string             `json:"storageClass"`
}

// Execute writes a file to a role. A retry that finds the same checksum at the key
// has nothing to do, so a retried backup never pays for a second write. With
// verifyChecksum the driver checks at write time, and a Stat confirms it, so a backup
// is verified without ever reading the archive back (I186).
func (n putNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p putParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	if err := parseRole(p.Storage); err != nil {
		return node.Result{}, err
	}
	if err := parseKey(p.Key); err != nil {
		return node.Result{}, err
	}
	input, err := env.OpenInput(ctx, p.Source, domain.RoleArchive)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	sum, size, err := provenance.HashFile(ctx, input.Path)
	if err != nil {
		return node.Result{}, ioError(err)
	}
	if input.SHA256 != "" && input.SHA256 != sum {
		return node.Result{}, node.Transient(errs.New(errs.CodeUnavailable, "the source changed while it was read"))
	}
	origin, _, err := provenance.Read(input.Path)
	if err != nil {
		return node.Result{}, ioError(err)
	}
	storage, err := env.Storage(ctx, p.Storage)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	existing, err := storage.Stat(ctx, p.Key)
	if err != nil && !errs.IsCode(err, errs.CodeNotFound) {
		return node.Result{}, nodeutil.Classify(err)
	}
	if err == nil && existing.SHA256 == sum && existing.SizeBytes == size {
		// With verifyChecksum the record alone is not enough: this asks the driver, which
		// on gcs still reads nothing (I186). A failed check writes the object again.
		res, verifyErr := storage.Verify(ctx, p.Key, sum)
		switch {
		case !p.VerifyChecksum, verifyErr == nil && res.OK:
			env.Logger().Info("the object is already stored with this checksum", "key", p.Key)
			return node.JSONResult(stored(p.Storage, existing, p.VerifyChecksum, origin.Profile))
		case verifyErr != nil && !errs.IsCode(verifyErr, errs.CodeCapabilityUnsupported):
			return node.Result{}, nodeutil.Classify(verifyErr)
		}
	}
	f, err := os.Open(input.Path)
	if err != nil {
		return node.Result{}, ioError(err)
	}
	defer func() { _ = f.Close() }()
	contentType := p.ContentType
	if contentType == "" {
		contentType = nodeutil.ContentType(p.Key)
	}
	opts := connector.PutOptions{
		SizeBytes:      size,
		ContentType:    contentType,
		StorageClass:   p.StorageClass,
		IdempotencyKey: in.IdempotencyKey,
		Progress:       progressFor(env, size, "storing "+string(p.Storage)),
	}
	if p.VerifyChecksum {
		opts.SHA256 = sum
	}
	info, err := storage.Put(ctx, p.Key, f, opts)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	if p.VerifyChecksum {
		check, err := storage.Stat(ctx, p.Key)
		if err != nil {
			return node.Result{}, nodeutil.Classify(err)
		}
		if check.SHA256 != sum || check.SizeBytes != size {
			return node.Result{}, node.Transient(errs.New(errs.CodeUnavailable, "the stored object does not match its checksum"))
		}
	}
	return node.JSONResult(stored(p.Storage, info, p.VerifyChecksum, origin.Profile))
}

// ioError classifies a failed local file operation: a full disk or a lost volume can
// heal, so it is worth a retry. A cancellation passes untouched.
func ioError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return node.Transient(errs.Wrap(errs.CodeUnavailable, err, "a local file operation failed"))
}

// ---- storage.get ----

type getNode struct{ nodeutil.Base }

type getParams struct {
	Source             string               `json:"source"`
	ForbidStorageRoles []domain.StorageRole `json:"forbidStorageRoles"`
	AllowArchive       bool                 `json:"allowArchive"`
}

// artifact is the catalog's artifact.
type artifact struct {
	File      string `json:"file"`
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
}

// Execute reads an object through the verified cache into the job's scratch, and
// records its origin beside it, which is what lets a later node trust the copy (I185).
func (n getNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p getParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	uri, err := domain.ParseInputURI(p.Source)
	if err != nil {
		return node.Result{}, errs.Wrap(errs.CodeInvalidArgument, err, "invalid source reference")
	}
	forbid := slices.Clone(p.ForbidStorageRoles)
	switch {
	case uri.Kind == domain.URIStorage && uri.Role == domain.RoleArchive:
		if err := archiveAllowed(env, uri.Role, p.AllowArchive); err != nil {
			return node.Result{}, err
		}
	default:
		forbid = append(forbid, domain.RoleArchive)
	}
	input, err := env.OpenInput(ctx, p.Source, forbid...)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	name := "object"
	if uri.Kind == domain.URIStorage {
		name = path.Base(uri.Key)
	}
	dst, err := env.Scratch().Path(name)
	if err != nil {
		return node.Result{}, err
	}
	if err := copyFile(ctx, input.Path, dst); err != nil {
		return node.Result{}, ioError(err)
	}
	sum := input.SHA256
	size := input.Size
	if sum == "" {
		sum, size, err = provenance.HashFile(ctx, dst)
		if err != nil {
			return node.Result{}, ioError(err)
		}
	}
	origin := provenance.Origin{SHA256: sum}
	if uri.Kind == domain.URIStorage {
		origin.Role = uri.Role
		origin.Key = uri.Key
	}
	if err := provenance.Write(dst, origin); err != nil {
		return node.Result{}, ioError(err)
	}
	return node.JSONResult(artifact{File: env.Scratch().URI(name), SHA256: sum, SizeBytes: size})
}

func copyFile(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	temp := nodeutil.TempFor(dst)
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.CopyBuffer(out, ctxReader{ctx, in}, make([]byte, 1<<20))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(temp)
		return err
	}
	return os.Rename(temp, dst)
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// ---- storage.verify ----

type verifyNode struct{ nodeutil.Base }

type verifyParams struct {
	Source       string `json:"source"`
	SHA256       string `json:"sha256"`
	AllowArchive bool   `json:"allowArchive"`
}

type verifyOutput struct {
	Verified  bool   `json:"verified"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	URI       string `json:"uri"`
}

// Execute checks an object's checksum. A mismatch is an answer, verified false, not a
// failure: verify-storage reports it. A missing object fails. The archive role needs
// allowArchive, and no permission, because no media leaves the driver (I186, I188).
func (n verifyNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p verifyParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	uri, err := parseStorageSource(p.Source)
	if err != nil {
		return node.Result{}, err
	}
	if uri.Role == domain.RoleArchive && !p.AllowArchive {
		return node.Result{}, errs.New(errs.CodeForbiddenStorageRole, "verifying the archive role needs allowArchive")
	}
	storage, err := env.Storage(ctx, uri.Role)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	want := p.SHA256
	if want == "" {
		info, err := storage.Stat(ctx, uri.Key)
		if err != nil {
			return node.Result{}, nodeutil.Classify(err)
		}
		want = info.SHA256
		if want == "" {
			return node.Result{}, errs.New(errs.CodeFailedPrecondition, "this object has no recorded checksum to verify against")
		}
	}
	res, err := storage.Verify(ctx, uri.Key, want)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	return node.JSONResult(verifyOutput{Verified: res.OK, SHA256: res.SHA256, SizeBytes: res.SizeBytes, URI: uri.String()})
}

// ---- storage.delete ----

type deleteNode struct{ nodeutil.Base }

type deleteParams struct {
	Source string `json:"source"`
}

type deleteOutput struct {
	Deleted bool `json:"deleted"`
}

// Execute deletes an object. A retry finding nothing is success, deleted false.
func (n deleteNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p deleteParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	uri, err := parseStorageSource(p.Source)
	if err != nil {
		return node.Result{}, err
	}
	storage, err := env.Storage(ctx, uri.Role)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	switch err := storage.Delete(ctx, uri.Key); {
	case err == nil:
		return node.JSONResult(deleteOutput{Deleted: true})
	case errs.IsCode(err, errs.CodeNotFound):
		return node.JSONResult(deleteOutput{Deleted: false})
	default:
		return node.Result{}, nodeutil.Classify(err)
	}
}

// ---- storage.copy ----

type copyNode struct{ nodeutil.Base }

type copyParams struct {
	Source       string             `json:"source"`
	Storage      domain.StorageRole `json:"storage"`
	Key          string             `json:"key"`
	AllowArchive bool               `json:"allowArchive"`
}

// Execute copies an object between roles: inside one instance with Copy, server side
// across instances when the driver can, otherwise streamed and verified by checksum.
// This is the only node besides storage.get that may read archive, and restore-archived
// the only workflow holding what it needs (D42).
func (n copyNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p copyParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	uri, err := parseStorageSource(p.Source)
	if err != nil {
		return node.Result{}, err
	}
	if err := parseRole(p.Storage); err != nil {
		return node.Result{}, err
	}
	if err := parseKey(p.Key); err != nil {
		return node.Result{}, err
	}
	if err := archiveAllowed(env, uri.Role, p.AllowArchive); err != nil {
		return node.Result{}, err
	}
	src, err := env.Storage(ctx, uri.Role)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	dst, err := env.Storage(ctx, p.Storage)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	info, err := src.Stat(ctx, uri.Key)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	if existing, err := dst.Stat(ctx, p.Key); err == nil && existing.SHA256 != "" && existing.SHA256 == info.SHA256 {
		return node.JSONResult(stored(p.Storage, existing, true, ""))
	}
	result, err := copyObject(ctx, env, src, dst, uri, p, info)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	verified := info.SHA256 != "" && result.SHA256 == info.SHA256
	if info.SHA256 != "" && !verified {
		return node.Result{}, node.Transient(errs.New(errs.CodeUnavailable, "the copy does not match its source's checksum"))
	}
	return node.JSONResult(stored(p.Storage, result, verified, ""))
}

func copyObject(ctx context.Context, env node.Env, src, dst connector.Storage, uri domain.InputURI, p copyParams, info connector.ObjectInfo) (connector.ObjectInfo, error) {
	if uri.Role == p.Storage {
		return dst.Copy(ctx, uri.Key, p.Key)
	}
	if copier, ok := dst.(connector.RoleCopier); ok {
		result, err := copier.CopyFrom(ctx, src, uri.Key, p.Key)
		if err == nil {
			return result, nil
		}
		if !errs.IsCode(err, errs.CodeCapabilityUnsupported) {
			return connector.ObjectInfo{}, err
		}
		env.Logger().Info("no server-side copy between these instances; streaming", "from", string(uri.Role), "to", string(p.Storage))
	}
	body, _, err := src.Get(ctx, uri.Key)
	if err != nil {
		return connector.ObjectInfo{}, err
	}
	defer func() { _ = body.Close() }()
	contentType := info.ContentType
	if contentType == "" {
		contentType = nodeutil.ContentType(p.Key)
	}
	return dst.Put(ctx, p.Key, body, connector.PutOptions{
		SHA256:      info.SHA256,
		SizeBytes:   info.SizeBytes,
		ContentType: contentType,
		Progress:    progressFor(env, info.SizeBytes, fmt.Sprintf("copying to %s", p.Storage)),
	})
}
