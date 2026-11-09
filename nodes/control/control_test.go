package controlnodes

import (
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/schemas"
)

func TestDescriptorsMatchTheCatalog(t *testing.T) {
	r := node.NewRegistry()
	Register(r)
	want := []string{wfdef.TypeFlowIf, wfdef.TypeFlowMerge, wfdef.TypeFlowWait, wfdef.TypeFlowForeach, wfdef.TypeWorkflowExecute, wfdef.TypeEventEmit}
	for _, typ := range want {
		impl, ok := r.Lookup(typ, 1)
		if !ok {
			t.Fatalf("%s is not registered", typ)
		}
		d := impl.Descriptor()
		if err := schemas.CheckDescriptor(d); err != nil {
			t.Error(err)
		}
		if d.Pool != node.PoolEngine {
			t.Errorf("%s runs in pool %s", typ, d.Pool)
		}
	}
	// What suspends can resume.
	for _, typ := range []string{wfdef.TypeFlowWait, wfdef.TypeFlowForeach, wfdef.TypeWorkflowExecute} {
		impl, _ := r.Lookup(typ, 1)
		if _, ok := impl.(node.Resumer); !ok {
			t.Errorf("%s suspends but cannot resume", typ)
		}
	}
}

func TestForeachLimits(t *testing.T) {
	p := foreachParams{Items: make([]any, 3)}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	if p.ItemInput != "item" || p.Concurrency != DefaultConcurrency || p.MaxItems != DefaultMaxItems {
		t.Fatalf("defaults %+v", p)
	}
	p = foreachParams{Items: make([]any, 3), Concurrency: 1000, MaxItems: 2}
	err := p.normalize()
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("item limit: %v", err)
	}
	if p.Concurrency != MaxConcurrency {
		t.Fatalf("concurrency %d", p.Concurrency)
	}
	p = foreachParams{MaxItems: 1 << 20}
	if err := p.normalize(); err != nil || p.MaxItems != MaxItems {
		t.Fatalf("hard limit %d %v", p.MaxItems, err)
	}
}
