package storagenodes_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/workers/provenance"
	"github.com/rafaelp/vidyom/internal/workers/workertest"
	storagenodes "github.com/rafaelp/vidyom/nodes/storage"
	"github.com/rafaelp/vidyom/schemas"
)

var ctx = context.Background()

func nodeOf(t *testing.T, nodeType string) node.Node {
	t.Helper()
	registry := node.NewRegistry()
	storagenodes.Register(registry)
	n, ok := registry.Lookup(nodeType, 1)
	if !ok {
		t.Fatalf("%s is not registered", nodeType)
	}
	return n
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestEveryCatalogStorageNodeIsRegistered(t *testing.T) {
	registry := node.NewRegistry()
	storagenodes.Register(registry)
	catalog, err := schemas.NodeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range catalog.Nodes {
		if !strings.HasPrefix(entry.Type, "storage.") {
			continue
		}
		n, ok := registry.Lookup(entry.Type, entry.TypeVersion)
		if !ok {
			t.Errorf("%s is in the catalog and not registered", entry.Type)
			continue
		}
		if err := schemas.CheckDescriptor(n.Descriptor()); err != nil {
			t.Error(err)
		}
	}
}

// scratchFile writes a file into the job's scratch, as an earlier node would.
func scratchFile(t *testing.T, h *workertest.Harness, name, content string) string {
	t.Helper()
	env := h.Env("earlier", "test")
	p, err := env.Scratch().Path(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return env.Scratch().URI(name)
}

func TestPutVerifiesWithoutReadingTheArchiveBack(t *testing.T) {
	h := workertest.New(t)
	src := scratchFile(t, h, "raw.mov", "raw recording bytes")
	out := h.MustRun(ctx, nodeOf(t, "storage.put"), "backup", map[string]any{
		"storage": "archive", "source": src, "key": "originals/ORI-001/v1/original.mov", "verifyChecksum": true,
	})
	if out.Field("uri") != "storage://archive/originals/ORI-001/v1/original.mov" || out.Field("storage") != "archive" {
		t.Errorf("%+v", out.Output)
	}
	if out.Field("sha256") != sum("raw recording bytes") || out.Output["verified"] != true {
		t.Errorf("%+v", out.Output)
	}
	if out.Number("size_bytes") != float64(len("raw recording bytes")) {
		t.Errorf("%+v", out.Output)
	}
	location, _ := out.Output["location"].(map[string]any)
	if location["driver"] != "local" || location["generation"] == nil {
		t.Errorf("location %+v (R30)", location)
	}
	if reads := h.ArchiveReads(); len(reads) != 0 {
		t.Fatalf("a backup read media from archive: %+v", reads)
	}
}

func TestPutRetrySkipsTheUpload(t *testing.T) {
	// The retry's check counts as verify, so it is still not a media read (I188).
	h := workertest.New(t)
	src := scratchFile(t, h, "raw.mov", "raw")
	params := map[string]any{"storage": "archive", "source": src, "key": "k", "verifyChecksum": true}
	first := h.MustRun(ctx, nodeOf(t, "storage.put"), "backup", params)
	second := h.MustRun(ctx, nodeOf(t, "storage.put"), "backup", params)
	if first.Field("sha256") != second.Field("sha256") {
		t.Fatal("checksums differ")
	}
	puts := 0
	for _, entry := range h.Access() {
		if entry.Op == "put" {
			puts++
		}
	}
	if puts != 1 {
		t.Fatalf("%d writes for one object", puts)
	}
}

// A retry that finds a damaged object of the same size must not call it verified.
func TestPutRetryRewritesADamagedObject(t *testing.T) {
	h := workertest.New(t)
	src := scratchFile(t, h, "raw.mov", "raw recording")
	params := map[string]any{"storage": "master", "source": src, "key": "k", "verifyChecksum": true}
	h.MustRun(ctx, nodeOf(t, "storage.put"), "n", params)
	stored := filepath.Join(h.Root, "roles", "master", "acme", "k")
	if err := os.WriteFile(stored, []byte("RAW RECORDING"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.MustRun(ctx, nodeOf(t, "storage.put"), "n", params)
	data, err := os.ReadFile(stored)
	if err != nil || string(data) != "raw recording" {
		t.Fatalf("%q %v", data, err)
	}
}

func TestPutReplacesDifferentContent(t *testing.T) {
	h := workertest.New(t)
	a := scratchFile(t, h, "a", "first")
	b := scratchFile(t, h, "b", "second")
	h.MustRun(ctx, nodeOf(t, "storage.put"), "n1", map[string]any{"storage": "master", "source": a, "key": "k"})
	out := h.MustRun(ctx, nodeOf(t, "storage.put"), "n2", map[string]any{"storage": "master", "source": b, "key": "k"})
	if out.Field("sha256") != sum("second") {
		t.Fatalf("%+v", out.Output)
	}
}

func TestPutCarriesTheProfileFromTheOrigin(t *testing.T) {
	h := workertest.New(t)
	src := scratchFile(t, h, "master.mp4", "encoded")
	if err := provenance.Write(h.ScratchPath(src), provenance.Origin{Profile: "master-1080p@1", Family: "master"}); err != nil {
		t.Fatal(err)
	}
	out := h.MustRun(ctx, nodeOf(t, "storage.put"), "store_master", map[string]any{"storage": "master", "source": src, "key": "k"})
	if out.Field("profile") != "master-1080p@1" {
		t.Fatalf("%+v", out.Output)
	}
}

func TestPutRefusesArchiveAsASourceAndBadInput(t *testing.T) {
	h := workertest.New(t)
	h.PutFile(domain.RoleArchive, "raw", writeTemp(t, "raw"))
	n := nodeOf(t, "storage.put")
	_, err := h.Run(ctx, n, "x", map[string]any{"storage": "master", "source": "storage://archive/raw", "key": "k"})
	if !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
		t.Errorf("archive source: %v", err)
	}
	src := scratchFile(t, h, "a", "a")
	_, err = h.Run(ctx, n, "x", map[string]any{"storage": "master", "source": src, "key": "../escape"})
	if !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("bad key: %v", err)
	}
	if _, err = h.Run(ctx, n, "x", map[string]any{"storage": "master", "source": "import:youtube/abc", "key": "k"}); !errs.IsCode(err, errs.CodeNotImplemented) {
		t.Errorf("import: %v", err)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPutFromServerAndUpload(t *testing.T) {
	h := workertest.New(t)
	root := t.TempDir()
	h.Deps.IngestRoots = []string{root}
	inside := filepath.Join(root, "class.mp4")
	if err := os.WriteFile(inside, []byte("from the server"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := nodeOf(t, "storage.put")
	out := h.MustRun(ctx, n, "a", map[string]any{"storage": "master", "source": "server:" + inside, "key": "a"})
	if out.Field("sha256") != sum("from the server") {
		t.Fatalf("%+v", out.Output)
	}
	if _, err := h.Run(ctx, n, "b", map[string]any{"storage": "master", "source": "server:" + writeTemp(t, "outside"), "key": "b"}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Errorf("path outside the ingest roots: %v", err)
	}

	upload := id.New()
	if err := os.WriteFile(filepath.Join(h.Deps.SpoolDir, "acme", string(upload)), []byte("uploaded"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = h.MustRun(ctx, n, "c", map[string]any{"storage": "master", "source": "upload:" + string(upload), "key": "c"})
	if out.Field("sha256") != sum("uploaded") {
		t.Fatalf("%+v", out.Output)
	}
	if _, err := h.Run(ctx, n, "d", map[string]any{"storage": "master", "source": "upload:" + string(id.New()), "key": "d"}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("missing upload: %v", err)
	}
	if _, err := h.Run(ctx, n, "e", map[string]any{"storage": "master", "source": "upload:UPL-001", "key": "e"}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Errorf("friendly upload (I183): %v", err)
	}
}

func TestGetGoesThroughTheCacheAndRecordsTheOrigin(t *testing.T) {
	h := workertest.New(t)
	info := h.PutFile(domain.RoleMaster, "originals/ORI-001/v1/master.mp4", writeTemp(t, "working master"))
	n := nodeOf(t, "storage.get")
	out := h.MustRun(ctx, n, "fetch", map[string]any{"source": "storage://master/originals/ORI-001/v1/master.mp4"})
	if out.Field("sha256") != info.SHA256 || out.Number("size_bytes") != float64(info.SizeBytes) {
		t.Fatalf("%+v", out.Output)
	}
	path := h.ScratchPath(out.Field("file"))
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "working master" {
		t.Fatalf("%q %v", data, err)
	}
	origin, known, err := provenance.Read(path)
	if err != nil || !known || origin.Role != domain.RoleMaster || origin.SHA256 != info.SHA256 || origin.Key != "originals/ORI-001/v1/master.mp4" {
		t.Fatalf("origin %+v %v %v", origin, known, err)
	}
	entries, err := os.ReadDir(filepath.Join(h.Root, "cache", "acme"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("cache entries %v %v", entries, err)
	}
	reads := len(h.Reads(domain.RoleMaster))
	h.MustRun(ctx, n, "fetch2", map[string]any{"source": "storage://master/originals/ORI-001/v1/master.mp4"})
	if got := len(h.Reads(domain.RoleMaster)); got != reads {
		t.Fatalf("the second read went to storage: %d, then %d", reads, got)
	}
}

func TestPlaintextInputCopyDiesWithTheTask(t *testing.T) {
	h := workertest.New(t)
	h.PutFile(domain.RoleMaster, "k", writeTemp(t, "master"))
	env := h.Env("worker", "test")
	input, err := env.OpenInput(ctx, "storage://master/k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(input.Path); err != nil {
		t.Fatal(err)
	}
	if input.Role != domain.RoleMaster || input.SHA256 != sum("master") {
		t.Fatalf("%+v", input)
	}
	env.Close()
	if _, err := os.Stat(input.Path); !os.IsNotExist(err) {
		t.Fatalf("the plaintext copy survived the task: %v", err)
	}
}

func TestGetRefusesArchiveWithoutBothConditions(t *testing.T) {
	h := workertest.New(t)
	h.PutFile(domain.RoleArchive, "raw", writeTemp(t, "raw"))
	n := nodeOf(t, "storage.get")
	source := "storage://archive/raw"

	_, err := h.Run(ctx, n, "a", map[string]any{"source": source})
	if !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
		t.Errorf("no allowArchive: %v", err)
	}
	_, err = h.Run(ctx, n, "b", map[string]any{"source": source, "allowArchive": true})
	if !errs.IsCode(err, errs.CodeForbidden) {
		t.Errorf("no permission: %v", err)
	}
	if reads := h.ArchiveReads(); len(reads) != 0 {
		t.Fatalf("archive was read: %+v", reads)
	}
	h.Permissions = []domain.Permission{domain.PermStorageArchiveRestore}
	out := h.MustRun(ctx, n, "c", map[string]any{"source": source, "allowArchive": true})
	origin, known, err := provenance.Read(h.ScratchPath(out.Field("file")))
	if err != nil || !known || origin.Role != domain.RoleArchive {
		t.Fatalf("origin %+v %v %v", origin, known, err)
	}
	if reads := h.ArchiveReads(); len(reads) != 1 {
		t.Fatalf("the permitted read did not show: %+v", reads)
	}
}

func TestGetHonorsForbidStorageRoles(t *testing.T) {
	h := workertest.New(t)
	h.PutFile(domain.RoleProxy, "p", writeTemp(t, "proxy"))
	_, err := h.Run(ctx, nodeOf(t, "storage.get"), "a", map[string]any{"source": "storage://proxy/p", "forbidStorageRoles": []string{"proxy"}})
	if !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
		t.Fatalf("%v", err)
	}
}

func TestVerify(t *testing.T) {
	h := workertest.New(t)
	info := h.PutFile(domain.RoleMaster, "k", writeTemp(t, "content"))
	n := nodeOf(t, "storage.verify")
	out := h.MustRun(ctx, n, "a", map[string]any{"source": "storage://master/k"})
	if out.Output["verified"] != true || out.Field("sha256") != info.SHA256 || out.Field("uri") != "storage://master/k" {
		t.Fatalf("%+v", out.Output)
	}
	out = h.MustRun(ctx, n, "b", map[string]any{"source": "storage://master/k", "sha256": sum("other")})
	if out.Output["verified"] != false {
		t.Fatalf("a wrong checksum verified: %+v", out.Output)
	}
	if _, err := h.Run(ctx, n, "c", map[string]any{"source": "storage://master/missing"}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("missing object: %v", err)
	}
	if _, err := h.Run(ctx, n, "d", map[string]any{"source": "server:/data/x"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("not storage: %v", err)
	}
	h.PutFile(domain.RoleArchive, "raw", writeTemp(t, "raw"))
	if _, err := h.Run(ctx, n, "e", map[string]any{"source": "storage://archive/raw"}); !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
		t.Errorf("archive without allowArchive: %v", err)
	}
	out = h.MustRun(ctx, n, "f", map[string]any{"source": "storage://archive/raw", "allowArchive": true})
	if out.Output["verified"] != true {
		t.Fatalf("%+v", out.Output)
	}
	if reads := h.ArchiveReads(); len(reads) != 0 {
		t.Fatalf("a verify counted as a media read: %+v", reads)
	}
}

func TestDeleteMissingIsSuccess(t *testing.T) {
	h := workertest.New(t)
	h.PutFile(domain.RoleProxy, "p", writeTemp(t, "proxy"))
	n := nodeOf(t, "storage.delete")
	if out := h.MustRun(ctx, n, "a", map[string]any{"source": "storage://proxy/p"}); out.Output["deleted"] != true {
		t.Fatalf("%+v", out.Output)
	}
	if out := h.MustRun(ctx, n, "a", map[string]any{"source": "storage://proxy/p"}); out.Output["deleted"] != false {
		t.Fatalf("%+v", out.Output)
	}
}

func TestCopyBetweenRolesAndArchiveRules(t *testing.T) {
	h := workertest.New(t)
	raw := h.PutFile(domain.RoleArchive, "originals/ORI-001/v1/original.mov", writeTemp(t, "raw"))
	h.PutFile(domain.RoleMaster, "m", writeTemp(t, "master"))
	n := nodeOf(t, "storage.copy")

	// Copying within reach of nothing archived is ordinary.
	out := h.MustRun(ctx, n, "a", map[string]any{"source": "storage://master/m", "storage": "proxy", "key": "m-copy"})
	if out.Field("storage") != "proxy" || out.Output["verified"] != true || out.Field("sha256") != sum("master") {
		t.Fatalf("%+v", out.Output)
	}
	out = h.MustRun(ctx, n, "b", map[string]any{"source": "storage://master/m", "storage": "master", "key": "m-same-role"})
	if out.Field("key") != "m-same-role" {
		t.Fatalf("%+v", out.Output)
	}

	archive := map[string]any{"source": "storage://archive/originals/ORI-001/v1/original.mov", "storage": "master", "key": "restored"}
	if _, err := h.Run(ctx, n, "c", archive); !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
		t.Errorf("archive without allowArchive: %v", err)
	}
	archive["allowArchive"] = true
	if _, err := h.Run(ctx, n, "d", archive); !errs.IsCode(err, errs.CodeForbidden) {
		t.Errorf("archive without permission: %v", err)
	}
	if reads := h.ArchiveReads(); len(reads) != 0 {
		t.Fatalf("archive was read: %+v", reads)
	}
	h.Permissions = []domain.Permission{domain.PermStorageArchiveRestore}
	out = h.MustRun(ctx, n, "e", archive)
	if out.Field("sha256") != raw.SHA256 || out.Output["verified"] != true {
		t.Fatalf("%+v", out.Output)
	}
	gets := len(h.ArchiveReads())
	out = h.MustRun(ctx, n, "f", archive)
	if out.Output["verified"] != true || len(h.ArchiveReads()) != gets {
		t.Fatalf("a repeated restore read archive again: %+v", out.Output)
	}
}

func TestUsageIsMeteredByTheEnvironment(t *testing.T) {
	h := workertest.New(t)
	h.PutFile(domain.RoleMaster, "m", writeTemp(t, strings.Repeat("x", 4096)))
	env := h.Env("usage", "storage.get")
	if _, err := env.OpenInput(ctx, "storage://master/m"); err != nil {
		t.Fatal(err)
	}
	s, err := env.Storage(ctx, domain.RoleProxy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "out", strings.NewReader(strings.Repeat("y", 1024)), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	records := env.Usage()
	if len(records) != 2 {
		t.Fatalf("%+v", records)
	}
	if records[0].Operation != "storage.egress" || records[0].Provider != "local" || records[0].Unit != "GB" || records[0].Quantity != 4096/1e9 {
		t.Errorf("%+v", records[0])
	}
	if records[1].Operation != "storage.ingress" || records[1].Quantity != 1024/1e9 {
		t.Errorf("%+v", records[1])
	}
	if records[0].EstimatedMinor != 0 || records[0].TenantID != "acme" || records[0].NodeID != "usage" {
		t.Errorf("%+v", records[0])
	}
}
