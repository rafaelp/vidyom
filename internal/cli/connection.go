package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

func (a *app) config() (*clientConfig, error) {
	if a.cfg != nil {
		return a.cfg, nil
	}
	dir, err := configDir(a.opts.ConfigDir, a.opts.Getenv)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "no client configuration directory")
	}
	cfg, err := loadConfig(dir)
	if err != nil {
		return nil, err
	}
	a.cfg, a.cfgDir = cfg, dir
	return cfg, nil
}

func (a *app) saveConfig() error {
	if a.cfg == nil {
		return nil
	}
	return a.cfg.save(a.cfgDir)
}

// implicitEntry is the same-host server used when nothing is configured (I162).
func (a *app) implicitEntry() (*serverEntry, error) {
	socket := a.opts.Getenv("VIDYOM_SOCKET")
	if socket == "" {
		socket = defaultSocket(a.opts.UserHomeDir)
	}
	if socket == "" {
		return nil, errs.New(errs.CodeFailedPrecondition, "no server is configured; add one with 'vidyom server add <name> --url https://... --fingerprint <sha256>'")
	}
	return &serverEntry{Kind: apiclient.KindUnix, Socket: socket}, nil
}

// resolveServer picks the server for this command. It returns its name, its entry and
// whether the entry is implicit (not yet in the configuration).
func (a *app) resolveServer() (string, *serverEntry, bool, error) {
	cfg, err := a.config()
	if err != nil {
		return "", nil, false, err
	}
	name := a.serverFlag
	if name == "" {
		name = cfg.Current
	}
	if name == "" {
		name = implicitServer
	}
	if entry, ok := cfg.Servers[name]; ok {
		return name, entry, false, nil
	}
	if name != implicitServer {
		return "", nil, false, errs.Newf(errs.CodeNotFound, "no server named %q; see 'vidyom server list'", name)
	}
	entry, err := a.implicitEntry()
	if err != nil {
		return "", nil, false, err
	}
	return name, entry, true, nil
}

// currentTenant applies --tenant over the server's current tenant (§11.1). An empty
// --tenant= drops the header on optional routes, for a system admin with no
// membership in the current tenant (I121).
func (a *app) currentTenant(entry *serverEntry) (string, error) {
	tenant := entry.Tenant
	if a.tenantFlagSet {
		tenant = a.tenantFlag
	}
	if tenant != "" {
		if err := domain.TenantID(tenant).Validate(); err != nil {
			return "", usagef("%v", err)
		}
	}
	return tenant, nil
}

// newClient builds a client for a server entry with this command's tenant.
func (a *app) newClient(name string, entry *serverEntry, tenant string) (*apiclient.Client, error) {
	endpoint := entry.endpoint()
	endpoint.SSHBinary = a.opts.SSHBinary
	return apiclient.New(apiclient.Options{
		Endpoint: endpoint,
		Tenant:   tenant,
		Token:    a.tokenFunc(name, entry),
		Sleep:    a.opts.Sleep,
	})
}

// api returns the client for this command, built once.
func (a *app) api() (*apiclient.Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	name, entry, _, err := a.resolveServer()
	if err != nil {
		return nil, err
	}
	tenant, err := a.currentTenant(entry)
	if err != nil {
		return nil, err
	}
	client, err := a.newClient(name, entry, tenant)
	if err != nil {
		return nil, err
	}
	a.client = client
	return client, nil
}

func (a *app) closeClient() {
	if a.client != nil {
		_ = a.client.Close()
	}
}

// invoke runs one call on this command's client.
func invoke[T any](ctx context.Context, a *app, call apiclient.Call) (T, error) {
	var zero T
	client, err := a.api()
	if err != nil {
		return zero, err
	}
	a.applyKey(&call)
	return apiclient.Invoke[T](ctx, client, call)
}

// applyKey uses --idempotency-key on a single mutating call. A command that sends
// several derives one per call, because the server refuses one key on two different
// requests (I122).
func (a *app) applyKey(call *apiclient.Call) {
	if a.idempotencyKey != "" && call.IdempotencyKey == "" {
		call.IdempotencyKey = a.idempotencyKey
	}
}

// keyFor derives this command's key for one of several mutating calls.
func (a *app) keyFor(parts ...string) string {
	if a.idempotencyKey == "" {
		return ""
	}
	return apiclient.DerivedKey(append([]string{a.idempotencyKey}, parts...)...)
}

// listFlags are the common flags of list commands.
type listFlags struct {
	limit  int
	cursor string
	all    bool
}

func (f *listFlags) add(cmd *cobra.Command) {
	cmd.Flags().IntVar(&f.limit, "limit", 0, "maximum number of items")
	cmd.Flags().StringVar(&f.cursor, "cursor", "", "continue after this cursor")
	cmd.Flags().BoolVar(&f.all, "all", false, "follow cursors to the end of the list")
}

func (f *listFlags) page() api.PageQuery { return api.PageQuery{Limit: f.limit, Cursor: f.cursor} }

// fetchList reads one page, or every page with --all. Items are never nil, so
// --json always holds an array.
func fetchList[T any](ctx context.Context, a *app, call apiclient.Call, f *listFlags) (api.List[T], error) {
	client, err := a.api()
	if err != nil {
		return api.List[T]{}, err
	}
	var list api.List[T]
	if f != nil && f.all {
		list, err = apiclient.All[T](ctx, client, call, 0)
	} else {
		list, err = apiclient.Items[T](ctx, client, call)
	}
	if err != nil {
		return api.List[T]{}, err
	}
	if list.Items == nil {
		list.Items = []T{}
	}
	return list, nil
}

// noteNextPage tells a person how to continue a list. --json has next_cursor.
func (a *app) noteNextPage(next string) {
	if next != "" {
		a.note("more results: --cursor %s", next)
	}
}
