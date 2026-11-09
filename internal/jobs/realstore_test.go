package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/persistence"
)

// The engine's tests take their store from one variable, so the same behavior can be
// checked on core's real SQLite store. That is what proves the fake's fidelity. With
// VIDYOM_JOBS_REAL_STORE=1 every test in this package runs on it; without, one smoke does.
func realStores(t *testing.T) repo.TenantStores {
	t.Helper()
	ctx := context.Background()
	stores, err := persistence.Open(ctx, persistence.Options{DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	for _, tenant := range []domain.TenantID{"acme", "globex"} {
		if _, err := stores.Tenants.Create(ctx, tenant); err != nil {
			t.Fatal(err)
		}
	}
	return stores.Tenants
}

func init() {
	if os.Getenv("VIDYOM_JOBS_REAL_STORE") != "" {
		defaultStores = realStores
	}
}

func TestEngineOnTheRealStore(t *testing.T) {
	h := newHarness(t, harnessOpts{stores: realStores(t)})
	asset := h.newAsset(domain.KindOriginal)
	h.add(strings.Replace(gateDoc, "KIND", "clip_selection", 1))
	job := h.run("gate", map[string]any{"asset": asset.ID})
	approval := h.pendingApproval(job)
	if _, err := h.engine.Approvals().Approve(context.Background(), scopeOf(editor), id.Ref(approval.ID), api.ApproveRequest{}); err != nil {
		t.Fatal(err)
	}
	done := h.waitStatus(job, domain.JobCompleted)
	var outputs map[string]any
	if err := json.Unmarshal(done.Outputs, &outputs); err != nil {
		t.Fatal(err)
	}
	if outputs["decision"] != "approved" {
		t.Fatalf("outputs %v", outputs)
	}
}
