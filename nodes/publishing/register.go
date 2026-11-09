// Package publishingnodes holds the nodes that drive destinations and the public
// library: publish.prepare, publish.upload, publish.await_processing,
// publish.finalize, publish.withdraw, publish.refresh, catalog.public.sync and
// mediacms.register. All run in pool daemon (I7), because they need the tenant
// database and vault credentials. The package also implements
// service.Publications, which shares their state rules.
//
// It links internal/publicexport, and through it sqlitedriver, so it belongs in
// vidyomd only. vidyom-worker must never register these nodes.
//
// See docs/plans/publishing.md and docs/decisions/publishing.md.
package publishingnodes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/publicexport"
	"github.com/rafaelp/vidyom/schemas"
)

// Exporter builds a tenant's public.db. *publicexport.Exporter implements it.
type Exporter interface {
	Export(ctx context.Context, req publicexport.Request) (publicexport.Result, error)
}

var _ Exporter = (*publicexport.Exporter)(nil)

// Options supplies what node.DaemonEnv does not offer (I238).
type Options struct {
	// Exporter is required by catalog.public.sync.
	Exporter Exporter
	// Connectors opens public and external catalog connectors.
	Connectors *connector.Registry
	// PollMin and PollMax bound publish.await_processing's backoff. Defaults are
	// 2 s and 60 s.
	PollMin time.Duration
	PollMax time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
}

// Default polling and timeout of publish.await_processing (I229).
const (
	defaultPollMin        = 2 * time.Second
	defaultPollMax        = 60 * time.Second
	defaultAwaitTimeout   = 6 * time.Hour
	catalogRegisterTries  = 3
	catalogRegisterBackup = 200 * time.Millisecond
)

// Types lists the node types this package registers.
var Types = []string{
	"publish.prepare", "publish.upload", "publish.await_processing", "publish.finalize",
	"publish.withdraw", "publish.refresh", "catalog.public.sync", "mediacms.register",
}

// Register adds every node. With no options the nodes still register, so workflows
// validate, and catalog.public.sync fails saying it is not wired.
func Register(r node.Registrar, opts ...Options) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.PollMin <= 0 {
		o.PollMin = defaultPollMin
	}
	if o.PollMax <= 0 {
		o.PollMax = defaultPollMax
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &shared{opts: o}
	r.RegisterNode(&prepareNode{newBase("publish.prepare", s)})
	r.RegisterNode(&uploadNode{newBase("publish.upload", s)})
	r.RegisterNode(&awaitNode{newBase("publish.await_processing", s)})
	r.RegisterNode(&finalizeNode{newBase("publish.finalize", s)})
	r.RegisterNode(&withdrawNode{newBase("publish.withdraw", s)})
	r.RegisterNode(&refreshNode{newBase("publish.refresh", s)})
	r.RegisterNode(&syncNode{newBase("catalog.public.sync", s)})
	r.RegisterNode(&mediacmsNode{newBase("mediacms.register", s)})
}

// shared is the state all nodes of one Register call have in common.
type shared struct {
	opts  Options
	locks recordLocks
}

// base gives a node its catalog descriptor.
type base struct {
	desc node.Descriptor
	*shared
}

func newBase(nodeType string, s *shared) base {
	d, err := schemas.DescriptorFor(nodeType, 1)
	if err != nil {
		panic(fmt.Sprintf("publishingnodes: %v", err))
	}
	return base{desc: d, shared: s}
}

// Descriptor returns exactly what the catalog declares.
func (b base) Descriptor() node.Descriptor { return b.desc }

// recordLocks stops two tasks acting on one publication at once in this daemon:
// YouTube has no server-side idempotency key, so this is what protects it (I222).
type recordLocks struct{ held sync.Map }

// tryLock returns an unlock function, or false when the record is busy.
func (l *recordLocks) tryLock(key string) (func(), bool) {
	if _, busy := l.held.LoadOrStore(key, struct{}{}); busy {
		return nil, false
	}
	return func() { l.held.Delete(key) }, true
}
