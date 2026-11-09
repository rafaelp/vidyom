package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

const (
	defaultLinkTTL = time.Hour
	// probeKey is under a name no workflow uses. It is written, read and deleted by
	// Test on operational roles only.
	probeKey = ".vidyom-probe/storage-test"
)

// Drivers finds a driver's factory. *connector.Registry satisfies it.
type Drivers interface {
	Storage(driver string) (connector.StorageFactory, error)
}

// Open opens one instance through its registered driver.
func Open(ctx context.Context, drivers Drivers, p connector.StorageParams) (connector.Storage, error) {
	factory, err := drivers.Storage(p.Instance.Driver)
	if err != nil {
		return nil, err
	}
	return factory(ctx, p)
}

// AssetFiles is what Verify and ProxyLink need from the catalog. service.Assets
// satisfies it.
type AssetFiles interface {
	Get(ctx context.Context, s service.Scope, ref id.Ref) (domain.Asset, error)
	Versions(ctx context.Context, s service.Scope, ref id.Ref) ([]domain.AssetVersion, error)
}

// Options configures a Resolver. Secrets and Assets are optional: without Secrets a
// credentialsRef cannot be resolved, and without Assets Verify and ProxyLink fail.
type Options struct {
	Drivers Drivers
	Tenants config.TenantSource
	Secrets func(domain.TenantID) connector.SecretResolver
	Assets  AssetFiles
	LinkTTL time.Duration
}

// Resolver opens and keeps instances per tenant and role, for the daemon.
type Resolver struct {
	opts Options

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	config  string
	storage connector.Storage
}

var (
	_ connector.StorageResolver = (*Resolver)(nil)
	_ service.Storages          = (*Resolver)(nil)
)

// New returns a Resolver.
func New(opts Options) *Resolver {
	if opts.LinkTTL <= 0 {
		opts.LinkTTL = defaultLinkTTL
	}
	return &Resolver{opts: opts, cache: map[string]cached{}}
}

// Storage returns the instance of a role for a tenant. An instance stays open while
// its configuration is unchanged. A configuration that breaks R28 fails closed.
func (r *Resolver) Storage(ctx context.Context, tenant domain.TenantID, role domain.StorageRole) (connector.Storage, error) {
	if err := tenant.Validate(); err != nil {
		return nil, errs.Wrap(errs.CodeInvalidArgument, err, "invalid tenant")
	}
	if !role.Valid() {
		return nil, errs.Newf(errs.CodeInvalidArgument, "unknown storage role %q", role)
	}
	inst, err := r.instance(ctx, tenant, role)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(inst)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot encode a storage instance")
	}
	key := string(tenant) + "/" + string(role)

	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.cache[key]; ok {
		if c.config == string(encoded) {
			return c.storage, nil
		}
		closeStorage(c.storage)
		delete(r.cache, key)
	}
	storage, err := Open(ctx, r.opts.Drivers, connector.StorageParams{
		Tenant:   tenant,
		Role:     role,
		Instance: inst,
		Secrets:  r.secretsFor(tenant),
	})
	if err != nil {
		return nil, err
	}
	r.cache[key] = cached{config: string(encoded), storage: storage}
	return storage, nil
}

func (r *Resolver) secretsFor(tenant domain.TenantID) connector.SecretResolver {
	if r.opts.Secrets == nil {
		return nil
	}
	return r.opts.Secrets(tenant)
}

func (r *Resolver) instance(ctx context.Context, tenant domain.TenantID, role domain.StorageRole) (config.StorageInstance, error) {
	if r.opts.Tenants == nil {
		return config.StorageInstance{}, errs.New(errs.CodeFailedPrecondition, "no tenant configuration source")
	}
	cfg, err := r.opts.Tenants.TenantConfig(ctx, tenant)
	if err != nil {
		return config.StorageInstance{}, err
	}
	if err := config.ValidateStorages(cfg.Storages); err != nil {
		return config.StorageInstance{}, errs.Wrap(errs.CodeFailedPrecondition, err, "the tenant's storage configuration is invalid: "+err.Error())
	}
	inst, ok := cfg.Storages.Roles[role]
	if !ok {
		return config.StorageInstance{}, errs.Newf(errs.CodeFailedPrecondition, "storage role %s is not configured", role)
	}
	return inst, nil
}

// Close closes every open instance.
func (r *Resolver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []error
	for key, c := range r.cache {
		if closer, ok := c.storage.(io.Closer); ok {
			all = append(all, closer.Close())
		}
		delete(r.cache, key)
	}
	return errors.Join(all...)
}

func closeStorage(s connector.Storage) {
	if closer, ok := s.(io.Closer); ok {
		_ = closer.Close()
	}
}

// List reports each role. Credentials never appear. A role that cannot open still
// shows its driver and usage, with no capabilities.
func (r *Resolver) List(ctx context.Context, s service.Scope) ([]api.StorageInfo, error) {
	out := make([]api.StorageInfo, 0, len(domain.StorageRoles))
	for _, role := range domain.StorageRoles {
		inst, err := r.instance(ctx, s.Tenant, role)
		if err != nil {
			return nil, err
		}
		info := api.StorageInfo{Role: role, Driver: inst.Driver, Usage: string(inst.Usage)}
		if storage, err := r.Storage(ctx, s.Tenant, role); err == nil {
			info.Capabilities = storage.Capabilities()
		}
		out = append(out, info)
	}
	return out, nil
}

// Test checks that a role works. It never writes to or reads from archive, because
// of minimum storage durations and retrieval costs (R29): a listing and a probe of
// a missing key are enough there. Operational roles also do a small round trip.
func (r *Resolver) Test(ctx context.Context, s service.Scope, role domain.StorageRole) (api.StorageTestResponse, error) {
	storage, err := r.Storage(ctx, s.Tenant, role)
	if err != nil {
		return api.StorageTestResponse{OK: false, Detail: errs.Public(err).Message}, nil
	}
	fail := func(step string, err error) (api.StorageTestResponse, error) {
		if ctx.Err() != nil {
			return api.StorageTestResponse{}, ctx.Err()
		}
		return api.StorageTestResponse{OK: false, Detail: step + ": " + errs.Public(err).Message}, nil
	}
	if _, err := storage.List(ctx, connector.ListOptions{Limit: 1}); err != nil {
		return fail("list", err)
	}
	if _, err := storage.Exists(ctx, probeKey); err != nil {
		return fail("exists", err)
	}
	if role == domain.RoleArchive || !storage.Capabilities().DirectAccess {
		return api.StorageTestResponse{OK: true, Detail: "listing and metadata only: no write or read on a backup role"}, nil
	}
	const content = "vidyom storage test"
	if _, err := storage.Put(ctx, probeKey, strings.NewReader(content), connector.PutOptions{ContentType: "text/plain"}); err != nil {
		return fail("put", err)
	}
	defer func() { _ = storage.Delete(context.WithoutCancel(ctx), probeKey) }()
	rc, _, err := storage.Get(ctx, probeKey)
	if err != nil {
		return fail("get", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return fail("get", err)
	}
	if string(data) != content {
		return api.StorageTestResponse{OK: false, Detail: "get returned different bytes than put wrote"}, nil
	}
	return api.StorageTestResponse{OK: true, Detail: "list, put, get and delete succeeded"}, nil
}

// Verify checks every file an asset's versions keep in a role. An asset with no
// file there is not reported as verified.
func (r *Resolver) Verify(ctx context.Context, s service.Scope, role domain.StorageRole, req api.StorageVerifyRequest) (api.StorageVerifyResponse, error) {
	if r.opts.Assets == nil {
		return api.StorageVerifyResponse{}, errs.New(errs.CodeFailedPrecondition, "storage verification has no catalog")
	}
	versions, err := r.opts.Assets.Versions(ctx, s, id.Ref(req.AssetID))
	if err != nil {
		return api.StorageVerifyResponse{}, err
	}
	storage, err := r.Storage(ctx, s.Tenant, role)
	if err != nil {
		return api.StorageVerifyResponse{}, err
	}
	var out api.StorageVerifyResponse
	for _, version := range versions {
		for _, file := range version.Files {
			if file.File.Storage != role {
				continue
			}
			out.Checked++
			if file.File.SHA256 == "" {
				out.Failed = append(out.Failed, file.File.Key)
				continue
			}
			res, err := storage.Verify(ctx, file.File.Key, file.File.SHA256)
			if err != nil {
				if ctx.Err() != nil {
					return api.StorageVerifyResponse{}, ctx.Err()
				}
				out.Failed = append(out.Failed, file.File.Key)
				continue
			}
			if !res.OK {
				out.Failed = append(out.Failed, file.File.Key)
			}
		}
	}
	out.OK = out.Checked > 0 && len(out.Failed) == 0
	return out, nil
}

// OpenAssetProxy implements service.ProxyReader: the proxy of an asset's current version.
func (r *Resolver) OpenAssetProxy(ctx context.Context, s service.Scope, ref id.Ref) (io.ReadCloser, connector.ObjectInfo, error) {
	if r.opts.Assets == nil {
		return nil, connector.ObjectInfo{}, errs.New(errs.CodeFailedPrecondition, "proxy content has no catalog")
	}
	asset, err := r.opts.Assets.Get(ctx, s, ref)
	if err != nil {
		return nil, connector.ObjectInfo{}, err
	}
	if asset.Proxy == nil || asset.Proxy.Key == "" {
		return nil, connector.ObjectInfo{}, errs.NotFound("proxy")
	}
	return r.openProxyKey(ctx, s, asset.Proxy.Key)
}

// OpenProxyURI implements service.ProxyReader. It reads role proxy and nothing else: an
// approval's stored reference must not be a way into master or archive.
func (r *Resolver) OpenProxyURI(ctx context.Context, s service.Scope, uri string) (io.ReadCloser, connector.ObjectInfo, error) {
	parsed, err := domain.ParseInputURI(uri)
	if err != nil {
		return nil, connector.ObjectInfo{}, errs.Wrap(errs.CodeInvalidArgument, err, "the preview reference is not valid")
	}
	if parsed.Kind != domain.URIStorage || parsed.Role != domain.RoleProxy {
		return nil, connector.ObjectInfo{}, errs.New(errs.CodeNotFound, "no preview is available in the proxy role")
	}
	return r.openProxyKey(ctx, s, parsed.Key)
}

func (r *Resolver) openProxyKey(ctx context.Context, s service.Scope, key string) (io.ReadCloser, connector.ObjectInfo, error) {
	storage, err := r.Storage(ctx, s.Tenant, domain.RoleProxy)
	if err != nil {
		return nil, connector.ObjectInfo{}, err
	}
	return storage.Get(ctx, key)
}

// ProxyLink returns a time-limited link to an asset's proxy for CLI review (D24).
// The link is never logged.
func (r *Resolver) ProxyLink(ctx context.Context, s service.Scope, ref id.Ref) (api.ProxyLinkResponse, error) {
	if r.opts.Assets == nil {
		return api.ProxyLinkResponse{}, errs.New(errs.CodeFailedPrecondition, "proxy links have no catalog")
	}
	asset, err := r.opts.Assets.Get(ctx, s, ref)
	if err != nil {
		return api.ProxyLinkResponse{}, err
	}
	if asset.Proxy == nil || asset.Proxy.Key == "" {
		return api.ProxyLinkResponse{}, errs.NotFound("proxy")
	}
	storage, err := r.Storage(ctx, s.Tenant, domain.RoleProxy)
	if err != nil {
		return api.ProxyLinkResponse{}, err
	}
	provider, ok := storage.(connector.LinkProvider)
	if !ok {
		return api.ProxyLinkResponse{}, errs.Newf(errs.CodeCapabilityUnsupported, "the %s driver cannot make temporary links", storage.Info().Driver)
	}
	expires := time.Now().Add(r.opts.LinkTTL)
	link, err := provider.TemporaryLink(ctx, asset.Proxy.Key, r.opts.LinkTTL)
	if err != nil {
		return api.ProxyLinkResponse{}, err
	}
	return api.ProxyLinkResponse{URL: link, ExpiresAt: expires}, nil
}
