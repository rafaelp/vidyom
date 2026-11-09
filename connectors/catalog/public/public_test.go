package public

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

func openDriver(t *testing.T, dir string) connector.PublicCatalog {
	t.Helper()
	reg := connector.NewRegistry()
	Register(reg)
	f, err := reg.PublicCatalog(Driver)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]string{"directory": dir})
	c, err := f(context.Background(), connector.CatalogParams{
		Tenant: "acme", Name: "publicLibrary",
		Config: config.CatalogInstance{Driver: Driver, Parameters: params},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func artifact(t *testing.T, tenant domain.TenantID, content string) connector.PublicArtifact {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.db")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return connector.PublicArtifact{
		Tenant: tenant, Path: path, SHA256: hex.EncodeToString(sum[:]),
		SizeBytes: int64(len(content)), SchemaVersion: 1,
	}
}

func TestShipReplacesAndClearIsRepeatable(t *testing.T) {
	dir := t.TempDir()
	c := openDriver(t, dir)
	ctx := context.Background()
	target := filepath.Join(dir, "acme", "public.db")

	first := artifact(t, "acme", "first version")
	receipt, err := c.Ship(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.SHA256 != first.SHA256 || receipt.Destination != Driver {
		t.Fatalf("receipt %+v", receipt)
	}
	if got, _ := os.ReadFile(target); string(got) != "first version" {
		t.Fatalf("target holds %q", got)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, want 0644: the content is public", info.Mode().Perm())
	}

	if _, err := c.Ship(ctx, artifact(t, "acme", "second version")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "second version" {
		t.Fatalf("target holds %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(target))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}

	for range 2 {
		if err := c.Clear(ctx, "acme"); err != nil {
			t.Fatalf("clear must be repeatable: %v", err)
		}
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("file still there: %v", err)
	}
}

func TestClearOfATenantNeverShipped(t *testing.T) {
	c := openDriver(t, t.TempDir())
	if err := c.Clear(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
}

func TestChecksumMismatchLeavesTheOldFile(t *testing.T) {
	dir := t.TempDir()
	c := openDriver(t, dir)
	ctx := context.Background()
	if _, err := c.Ship(ctx, artifact(t, "acme", "good")); err != nil {
		t.Fatal(err)
	}
	bad := artifact(t, "acme", "tampered")
	bad.SHA256 = "00"
	if _, err := c.Ship(ctx, bad); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "acme", "public.db")); string(got) != "good" {
		t.Fatalf("target holds %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "acme"))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestTenantsDoNotShareAFile(t *testing.T) {
	dir := t.TempDir()
	c := openDriver(t, dir)
	ctx := context.Background()
	if _, err := c.Ship(ctx, artifact(t, "acme", "acme data")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ship(ctx, artifact(t, "globex", "globex data")); err != nil {
		t.Fatal(err)
	}
	if err := c.Clear(ctx, "globex"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "acme", "public.db")); string(got) != "acme data" {
		t.Fatalf("acme holds %q", got)
	}
}

func TestRefusesBadTenantAndBadConfiguration(t *testing.T) {
	c := openDriver(t, t.TempDir())
	ctx := context.Background()
	if err := c.Clear(ctx, "../escape"); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Ship(ctx, artifact(t, "../escape", "x")); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("got %v", err)
	}

	for _, params := range []string{``, `{}`, `{"directory":"relative/path"}`, `{"directory":`} {
		_, err := open(ctx, connector.CatalogParams{Config: config.CatalogInstance{Parameters: json.RawMessage(params)}})
		if !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Fatalf("parameters %q: got %v", params, err)
		}
	}
}

func TestCancelledShipLeavesTheOldFile(t *testing.T) {
	dir := t.TempDir()
	c := openDriver(t, dir)
	if _, err := c.Ship(context.Background(), artifact(t, "acme", "good")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Ship(ctx, artifact(t, "acme", "new")); err == nil {
		t.Fatal("a cancelled ship must fail")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "acme", "public.db")); string(got) != "good" {
		t.Fatalf("target holds %q", got)
	}
}
