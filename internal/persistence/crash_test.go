package persistence

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/security/audit"
)

const crashHelperEnv = "VIDYOM_TEST_CRASH_DATA_DIR"

// TestCrashHelper is not a test: it is the process TestKilledDaemonKeepsWhatItAcknowledged
// kills. It writes in a loop and acknowledges each commit on stdout.
func TestCrashHelper(t *testing.T) {
	dir := os.Getenv(crashHelperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	ctx := context.Background()
	stores, err := Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	store, err := stores.Tenants.ForTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	for {
		var friendly id.Friendly
		err := store.InTx(ctx, func(tx repo.TenantRepos) error {
			var err error
			friendly, err = tx.IDs().Next(ctx, id.PrefixClip)
			if err != nil {
				return err
			}
			if err := tx.Assets().Create(ctx, domain.Asset{ID: friendly, ULID: id.New(), Kind: domain.KindClip, Title: "t"}); err != nil {
				return err
			}
			if _, err := tx.Outbox().Append(ctx, event.Envelope{Type: event.AssetCreated, AssetID: string(friendly)}); err != nil {
				return err
			}
			_, err = tx.Audit().Append(ctx, domain.AuditRecord{Actor: "a", Action: "asset.create", Resource: string(friendly), Result: "allowed"})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println(friendly)
	}
}

// §15.5 and D4: the tenant database is the source of truth, so a daemon killed in the
// middle of writing must keep everything it acknowledged, with each state change and
// its event and audit record whole, and the database must reopen. This proves a process
// crash, not a power failure, which needs the durable settings TestDurableSettings
// asserts.
func TestKilledDaemonKeepsWhatItAcknowledged(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	stores, err := Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stores.Tenants.Create(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := stores.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.v=false")
	cmd.Env = append(os.Environ(), crashHelperEnv+"="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var acknowledged []id.Friendly
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if id.IsFriendly(line) {
			acknowledged = append(acknowledged, id.Friendly(line))
		}
		if len(acknowledged) == 200 {
			break
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if len(acknowledged) != 200 {
		t.Fatalf("the writer acknowledged only %d commits", len(acknowledged))
	}

	stores, err = Open(ctx, Options{DataDir: dir, RequireExisting: true})
	if err != nil {
		t.Fatalf("the database must reopen after a kill: %v", err)
	}
	defer stores.Close()
	store, err := stores.Tenants.ForTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	for _, friendly := range acknowledged {
		if _, err := store.Assets().Get(ctx, id.Ref(friendly)); err != nil {
			t.Fatalf("%s was acknowledged and is gone: %v", friendly, err)
		}
	}

	// Whatever else committed before the kill is whole: one asset, one event and one
	// audit record each, and a chain that verifies.
	var assets int
	cursor := ""
	for {
		items, next, err := store.Assets().List(ctx, repo.AssetFilter{}, repo.Page{Limit: 500, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		assets += len(items)
		if next == "" {
			break
		}
		cursor = next
	}
	var events int
	var after int64
	for {
		entries, err := store.Outbox().Stream(ctx, after, 500)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			break
		}
		events += len(entries)
		after = entries[len(entries)-1].Seq
	}
	verified, err := audit.New(stores.Tenants, audit.Options{}).Verify(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !verified.OK {
		t.Fatalf("the chain does not verify after a kill: %+v", verified)
	}
	if assets < len(acknowledged) || events != assets || verified.Records != int64(assets) {
		t.Fatalf("a transaction was torn: %d assets, %d events, %d audit records", assets, events, verified.Records)
	}

	// IDs continue past everything that committed; none is reused.
	next, err := store.IDs().Next(ctx, id.PrefixClip)
	if err != nil {
		t.Fatal(err)
	}
	if next.Number() != uint64(assets)+1 {
		t.Fatalf("next = %s after %d clips", next, assets)
	}
	if _, err := store.Assets().Get(ctx, id.Ref(next)); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("%s must be free: %v", next, err)
	}
}
