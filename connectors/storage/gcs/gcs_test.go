package gcs_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"google.golang.org/api/option"

	"github.com/rafaelp/vidyom/connectors/storage/gcs"
	"github.com/rafaelp/vidyom/connectors/storage/storagetest"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// The image version matches the fake-gcs-server module in go.mod.
const fakeImage = "fsouza/fake-gcs-server:1.56.1"

// server is a running fake-gcs-server, in a container or in process.
type server struct {
	endpoint string
	client   *storage.Client
}

func newServer(t *testing.T, endpoint string) *server {
	t.Helper()
	client, err := storage.NewClient(context.Background(), option.WithEndpoint(endpoint), option.WithoutAuthentication(), storage.WithJSONReads())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &server{endpoint: endpoint, client: client}
}

func (s *server) createBucket(t *testing.T, name string) {
	t.Helper()
	if err := s.client.Bucket(name).Create(context.Background(), "vidyom-test", nil); err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
}

// inProcessServer runs fake-gcs-server inside the test binary, over real HTTP.
func inProcessServer(t *testing.T) *server {
	t.Helper()
	fake, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme:     "http",
		Host:       "127.0.0.1",
		Port:       freePort(t),
		PublicHost: "127.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Stop)
	return newServer(t, fake.URL()+"/storage/v1/")
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return uint16(l.Addr().(*net.TCPAddr).Port)
}

// containerServer runs the released image, with a vidyom-dev-media- name and a
// non-default port, and removes it when the test ends.
func containerServer(t *testing.T) *server {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found")
	}
	if err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").Run(); err != nil {
		t.Skip("docker is not running")
	}
	if err := exec.Command("docker", "image", "inspect", fakeImage).Run(); err != nil {
		if out, err := exec.Command("docker", "pull", fakeImage).CombinedOutput(); err != nil {
			t.Skipf("cannot pull %s: %v\n%s", fakeImage, err, out)
		}
	}
	port := strconv.Itoa(int(freePort(t)))
	name := fmt.Sprintf("vidyom-dev-media-gcs-%d-%s", os.Getpid(), port)
	address := "127.0.0.1:" + port
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", address+":4443", fakeImage,
		"-scheme", "http", "-port", "4443",
		"-public-host", address, "-external-url", "http://"+address,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	s := newServer(t, "http://"+address+"/storage/v1/")
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		it := s.client.Buckets(ctx, "vidyom-test")
		_, err := it.Next()
		cancel()
		if err == nil || strings.Contains(err.Error(), "no more items") {
			return s
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("fake-gcs-server did not start: %v\n%s", err, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

var unsafePrefix = regexp.MustCompile(`[^a-zA-Z0-9-]+`)

func prefixFor(t *testing.T) string { return unsafePrefix.ReplaceAllString(t.Name(), "-") + "/" }

func params(t *testing.T, srv *server, extra map[string]any) json.RawMessage {
	t.Helper()
	m := map[string]any{"endpoint": srv.endpoint}
	for k, v := range extra {
		m[k] = v
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func openGCS(t *testing.T, srv *server, tenant domain.TenantID, role domain.StorageRole, inst config.StorageInstance) *gcs.Storage {
	t.Helper()
	inst.Driver = gcs.Driver
	if inst.Bucket == "" {
		inst.Bucket = "videos-standard"
	}
	if inst.Parameters == nil {
		inst.Parameters = params(t, srv, nil)
	}
	s, err := gcs.Open(context.Background(), connector.StorageParams{Tenant: tenant, Role: role, Instance: inst})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.(*gcs.Storage).Close() })
	return s.(*gcs.Storage)
}

func runConformance(t *testing.T, srv *server) {
	t.Helper()
	srv.createBucket(t, "videos-standard")
	storagetest.Run(t, func(t *testing.T, tenant domain.TenantID) connector.Storage {
		return openGCS(t, srv, tenant, domain.RoleMaster, config.StorageInstance{Prefix: prefixFor(t), Usage: config.UsageOperational})
	}, storagetest.Options{
		TamperObject: func(t *testing.T, tenant domain.TenantID, key string, content []byte) {
			w := srv.client.Bucket("videos-standard").Object(prefixFor(t) + string(tenant) + "/" + key).NewWriter(context.Background())
			if _, err := w.Write(content); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		},
	})
}

func TestConformanceInProcess(t *testing.T) { runConformance(t, inProcessServer(t)) }

func TestConformanceInContainer(t *testing.T) { runConformance(t, containerServer(t)) }

func TestSpecificsInContainer(t *testing.T) { testSpecifics(t, containerServer(t)) }

func TestSpecificsInProcess(t *testing.T) { testSpecifics(t, inProcessServer(t)) }

func testSpecifics(t *testing.T, srv *server) {
	srv.createBucket(t, "videos-archive")
	srv.createBucket(t, "videos-standard")
	ctx := context.Background()

	t.Run("VerifyNeedsNoReadWhenTheServerChecksumHolds", func(t *testing.T) {
		s := openGCS(t, srv, "acme", domain.RoleArchive, config.StorageInstance{Bucket: "videos-archive", Prefix: prefixFor(t), StorageClass: "ARCHIVE"})
		content := []byte("a raw recording in an archive class")
		info, err := s.Put(ctx, "originals/ORI-001/v1/original.mov", bytes.NewReader(content), connector.PutOptions{SHA256: storagetest.Sum(content)})
		if err != nil {
			t.Fatal(err)
		}
		attrs, err := srv.client.Bucket("videos-archive").Object(prefixFor(t) + "acme/originals/ORI-001/v1/original.mov").Attrs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if attrs.StorageClass != "ARCHIVE" {
			t.Errorf("storage class %q", attrs.StorageClass)
		}
		if attrs.Metadata["vidyom-sha256"] != storagetest.Sum(content) || attrs.Metadata["vidyom-crc32c"] == "" {
			t.Errorf("metadata %v", attrs.Metadata)
		}
		res, err := s.Verify(ctx, info.Key, storagetest.Sum(content))
		if err != nil || !res.OK {
			t.Fatalf("%+v %v", res, err)
		}
	})

	t.Run("LargeUploadInChunks", func(t *testing.T) {
		s := openGCS(t, srv, "acme", domain.RoleMaster, config.StorageInstance{
			Prefix: prefixFor(t), Parameters: params(t, srv, map[string]any{"chunkSizeBytes": 256 << 10}),
		})
		content := storagetest.Fill(1 << 20)
		info, err := s.Put(ctx, "big", bytes.NewReader(content), connector.PutOptions{SHA256: storagetest.Sum(content)})
		if err != nil {
			t.Fatal(err)
		}
		if info.SizeBytes != 1<<20 {
			t.Fatalf("%+v", info)
		}
		rc, err := s.DownloadRange(ctx, "big", 1<<19, 1<<10)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !bytes.Equal(data, content[1<<19:1<<19+1<<10]) {
			t.Fatal("range across chunks differs")
		}
	})

	t.Run("WrongChecksumOnALargeUploadLeavesNothing", func(t *testing.T) {
		s := openGCS(t, srv, "acme", domain.RoleMaster, config.StorageInstance{
			Prefix: prefixFor(t), Parameters: params(t, srv, map[string]any{"chunkSizeBytes": 256 << 10}),
		})
		content := storagetest.Fill(1 << 20)
		if _, err := s.Put(ctx, "bad", bytes.NewReader(content), connector.PutOptions{SHA256: storagetest.Sum([]byte("other"))}); err == nil {
			t.Fatal("a wrong checksum must fail")
		}
		if ok, err := s.Exists(ctx, "bad"); err != nil || ok {
			t.Fatalf("object remained: %v %v", ok, err)
		}
	})

	t.Run("CopyBetweenBucketsIsServerSide", func(t *testing.T) {
		prefix := prefixFor(t)
		src := openGCS(t, srv, "acme", domain.RoleArchive, config.StorageInstance{Bucket: "videos-archive", Prefix: prefix})
		dst := openGCS(t, srv, "acme", domain.RoleMaster, config.StorageInstance{Bucket: "videos-standard", Prefix: prefix})
		content := []byte("restored by rewrite")
		if _, err := src.Put(ctx, "k", bytes.NewReader(content), connector.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		info, err := dst.CopyFrom(ctx, src, "k", "restored/k")
		if err != nil {
			t.Fatal(err)
		}
		if info.SHA256 != storagetest.Sum(content) {
			t.Fatalf("metadata did not travel: %+v", info)
		}
		if res, err := dst.Verify(ctx, "restored/k", storagetest.Sum(content)); err != nil || !res.OK {
			t.Fatalf("%+v %v", res, err)
		}
		if _, err := dst.CopyFrom(ctx, fakeStorage{}, "k", "x"); !errs.IsCode(err, errs.CodeCapabilityUnsupported) {
			t.Fatalf("copy from another driver: %v", err)
		}
	})

	t.Run("LocationRecordsGeneration", func(t *testing.T) {
		s := openGCS(t, srv, "acme", domain.RoleMaster, config.StorageInstance{Prefix: prefixFor(t)})
		info, err := s.Put(ctx, "k", strings.NewReader("x"), connector.PutOptions{})
		if err != nil {
			t.Fatal(err)
		}
		loc := info.Location
		if loc.Driver != "gcs" || loc.Bucket != "videos-standard" || loc.Generation == "" || !strings.HasSuffix(loc.Prefix, "acme/") {
			t.Fatalf("location %+v", loc)
		}
	})

	t.Run("MissingBucket", func(t *testing.T) {
		s := openGCS(t, srv, "acme", domain.RoleMaster, config.StorageInstance{Bucket: "no-such-bucket", Prefix: prefixFor(t)})
		if _, err := s.Put(ctx, "k", strings.NewReader("x"), connector.PutOptions{}); err == nil {
			t.Fatal("a write to a missing bucket must fail")
		}
	})
}

// TestVerifyReadsNoContent proves I186 by counting media requests through a proxy
// in front of the server: verifying an object written by this driver fetches only
// its attributes, which is what makes it safe on the ARCHIVE class.
func TestVerifyReadsNoContent(t *testing.T) {
	srv := inProcessServer(t)
	srv.createBucket(t, "videos-archive")
	backend, err := url.Parse(srv.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	var mediaReads atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Get("alt") == "media" {
			mediaReads.Add(1)
		}
		httputil.NewSingleHostReverseProxy(&url.URL{Scheme: backend.Scheme, Host: backend.Host}).ServeHTTP(w, r)
	}))
	defer proxy.Close()
	through := &server{endpoint: proxy.URL + "/storage/v1/"}
	s := openGCS(t, through, "acme", domain.RoleArchive, config.StorageInstance{Bucket: "videos-archive", Prefix: "verify/"})
	ctx := context.Background()
	content := []byte("archived bytes")
	if _, err := s.Put(ctx, "k", bytes.NewReader(content), connector.PutOptions{SHA256: storagetest.Sum(content)}); err != nil {
		t.Fatal(err)
	}
	mediaReads.Store(0)
	if res, err := s.Verify(ctx, "k", storagetest.Sum(content)); err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	if sum, err := s.Checksum(ctx, "k"); err != nil || sum != storagetest.Sum(content) {
		t.Fatalf("%s %v", sum, err)
	}
	if n := mediaReads.Load(); n != 0 {
		t.Fatalf("verify read the object %d times", n)
	}
	// An object not written by this driver has no record, so it must be read.
	w := srv.client.Bucket("videos-archive").Object("verify/acme/outside").NewWriter(ctx)
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Verify(ctx, "outside", storagetest.Sum(content)); err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	if mediaReads.Load() == 0 {
		t.Fatal("an object with no record was accepted without reading it")
	}
}

// An outage must surface as an error the node can classify as transient, not hang.
func TestOutageBecomesAnError(t *testing.T) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/storage/v1/", freePort(t))
	parameters, _ := json.Marshal(map[string]any{"endpoint": endpoint, "retryAttempts": 2, "stallTimeoutSeconds": 3})
	opened, err := gcs.Open(context.Background(), connector.StorageParams{
		Tenant: "acme", Role: domain.RoleMaster,
		Instance: config.StorageInstance{Driver: gcs.Driver, Bucket: "b", Parameters: parameters},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*gcs.Storage)
	defer func() { _ = s.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	wantUnavailable := func(what string, err error) {
		t.Helper()
		if !errs.IsCode(err, errs.CodeUnavailable) {
			t.Errorf("%s: %v", what, err)
		}
		if ctx.Err() != nil {
			t.Fatalf("%s only ended when the test's deadline did", what)
		}
	}
	_, err = s.Stat(ctx, "k")
	wantUnavailable("stat", err)
	wantUnavailable("delete", s.Delete(ctx, "k"))
	_, err = s.Put(ctx, "k", strings.NewReader("x"), connector.PutOptions{})
	wantUnavailable("put", err)
	_, _, err = s.Get(ctx, "k")
	wantUnavailable("get", err)
	_, err = s.List(ctx, connector.ListOptions{})
	wantUnavailable("list", err)
}

// A transfer that stops mid-way fails by itself: nobody's context would end it, while
// its heartbeat kept the node looking alive.
func TestStalledDownloadFails(t *testing.T) {
	srv := inProcessServer(t)
	srv.createBucket(t, "videos-standard")
	release := make(chan struct{})
	defer close(release)
	through := proxied(t, srv, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method == http.MethodGet && r.URL.Query().Get("alt") == "media" {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("partial"))
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return true
		}
		return false
	})
	s := openGCS(t, through, "acme", domain.RoleMaster, config.StorageInstance{
		Prefix: "stall/", Parameters: params(t, through, map[string]any{"stallTimeoutSeconds": 1, "retryAttempts": 1}),
	})
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader(strings.Repeat("x", 100)), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	rc, _, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	result := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(rc)
		result <- err
	}()
	select {
	case err := <-result:
		if !errs.IsCode(err, errs.CodeUnavailable) {
			t.Fatalf("%v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a stalled download hung")
	}
}

// A task's own cancellation must still read as a cancellation.
func TestCancellationPassesThrough(t *testing.T) {
	srv := inProcessServer(t)
	srv.createBucket(t, "videos-standard")
	s := openGCS(t, srv, "acme", domain.RoleMaster, config.StorageInstance{Prefix: "cancel/"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Stat(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

// proxied puts a recording proxy in front of a server.
func proxied(t *testing.T, srv *server, intercept func(w http.ResponseWriter, r *http.Request, body []byte) bool) *server {
	t.Helper()
	backend, err := url.Parse(srv.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: backend.Scheme, Host: backend.Host})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if intercept(w, r, body) {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	return &server{endpoint: proxy.URL + "/storage/v1/", client: srv.client}
}

// A rewrite must name the source's metadata itself: with a storage class in the body,
// the service may not carry it over, and the recorded checksum would be lost. The
// fake merges either way, so this checks what is sent.
func TestRewriteSendsTheSourcesMetadata(t *testing.T) {
	srv := inProcessServer(t)
	srv.createBucket(t, "videos-standard")
	var rewrites atomic.Int64
	var sent atomic.Pointer[map[string]any]
	through := proxied(t, srv, func(_ http.ResponseWriter, r *http.Request, body []byte) bool {
		if strings.Contains(r.URL.Path, "/rewriteTo/") {
			rewrites.Add(1)
			var doc map[string]any
			if json.Unmarshal(body, &doc) == nil {
				sent.Store(&doc)
			}
		}
		return false
	})
	s := openGCS(t, through, "acme", domain.RoleArchive, config.StorageInstance{Prefix: "rewrite/", StorageClass: "ARCHIVE"})
	ctx := context.Background()
	content := []byte("a backup")
	put, err := s.Put(ctx, "src", bytes.NewReader(content), connector.PutOptions{ContentType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := s.Copy(ctx, "src", "dst")
	if err != nil {
		t.Fatal(err)
	}
	if rewrites.Load() == 0 {
		t.Fatal("no rewrite passed through the proxy")
	}
	doc := *sent.Load()
	metadata, _ := doc["metadata"].(map[string]any)
	if metadata["vidyom-sha256"] != put.SHA256 || metadata["vidyom-crc32c"] == nil {
		t.Fatalf("the rewrite did not send the checksum: %+v", doc)
	}
	if doc["contentType"] != "video/mp4" || doc["storageClass"] != "ARCHIVE" {
		t.Fatalf("%+v", doc)
	}
	if copied.SHA256 != put.SHA256 || copied.ContentType != "video/mp4" {
		t.Fatalf("%+v", copied)
	}
}

// An object whose checksum could not be recorded could not be verified, so a failed
// write leaves none.
func TestFailedRecordLeavesNoObject(t *testing.T) {
	srv := inProcessServer(t)
	srv.createBucket(t, "videos-standard")
	var refused atomic.Int64
	through := proxied(t, srv, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method == http.MethodPatch {
			refused.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return true
		}
		return false
	})
	s := openGCS(t, through, "acme", domain.RoleMaster, config.StorageInstance{Prefix: "record/"})
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader("content"), connector.PutOptions{}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("%v", err)
	}
	if refused.Load() == 0 {
		t.Fatal("the record's update was never attempted")
	}
	if ok, err := s.Exists(ctx, "k"); err != nil || ok {
		t.Fatalf("an object with no record stayed: %v %v", ok, err)
	}
}

// fakeStorage is a source from another driver.
type fakeStorage struct{ connector.Storage }

func TestOpenFailsClosed(t *testing.T) {
	ctx := context.Background()
	cases := map[string]connector.StorageParams{
		"no bucket":                     {Tenant: "acme", Role: domain.RoleMaster, Instance: config.StorageInstance{Parameters: json.RawMessage(`{"endpoint":"http://127.0.0.1:1"}`)}},
		"no credential and no emulator": {Tenant: "acme", Role: domain.RoleMaster, Instance: config.StorageInstance{Bucket: "b"}},
		"credential with no resolver":   {Tenant: "acme", Role: domain.RoleMaster, Instance: config.StorageInstance{Bucket: "b", CredentialsRef: "gcs-main"}},
		"typo in parameters":            {Tenant: "acme", Role: domain.RoleMaster, Instance: config.StorageInstance{Bucket: "b", Parameters: json.RawMessage(`{"endpont":"x"}`)}},
		"plain http off loopback":       {Tenant: "acme", Role: domain.RoleMaster, Instance: config.StorageInstance{Bucket: "b", Parameters: json.RawMessage(`{"endpoint":"http://media.example.internal/"}`)}},
		"tiny chunk":                    {Tenant: "acme", Role: domain.RoleMaster, Instance: config.StorageInstance{Bucket: "b", Parameters: json.RawMessage(`{"endpoint":"http://127.0.0.1:1","chunkSizeBytes":1}`)}},
		"negative stall":                {Tenant: "acme", Role: domain.RoleMaster, Instance: config.StorageInstance{Bucket: "b", Parameters: json.RawMessage(`{"endpoint":"http://127.0.0.1:1","stallTimeoutSeconds":-1}`)}},
		"negative attempts":             {Tenant: "acme", Role: domain.RoleMaster, Instance: config.StorageInstance{Bucket: "b", Parameters: json.RawMessage(`{"endpoint":"http://127.0.0.1:1","retryAttempts":-1}`)}},
		"bad tenant":                    {Tenant: "../x", Role: domain.RoleMaster, Instance: config.StorageInstance{Bucket: "b", Parameters: json.RawMessage(`{"endpoint":"http://127.0.0.1:1"}`)}},
	}
	for name, p := range cases {
		if _, err := gcs.Open(ctx, p); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

type staticSecret []byte

func (s staticSecret) ResolveSecret(context.Context, string) ([]byte, error) { return s, nil }

func TestTemporaryLinkIsSignedAndCredentialsStayOut(t *testing.T) {
	srv := inProcessServer(t)
	srv.createBucket(t, "videos-standard")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	credential, _ := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "worker@media.example.internal", "private_key": string(pemKey),
	})
	ctx := context.Background()
	inst := config.StorageInstance{Driver: gcs.Driver, Bucket: "videos-standard", Prefix: "links/", CredentialsRef: "gcs-main", Parameters: params(t, srv, nil)}
	opened, err := gcs.Open(ctx, connector.StorageParams{Tenant: "acme", Role: domain.RoleProxy, Instance: inst, Secrets: staticSecret(credential)})
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*gcs.Storage)
	defer func() { _ = s.Close() }()
	if _, err := s.Put(ctx, "proxy.mp4", strings.NewReader("x"), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	link, err := s.TemporaryLink(ctx, "proxy.mp4", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	expires, err := strconv.Atoi(u.Query().Get("X-Goog-Expires"))
	if err != nil || expires < 3590 || expires > 3600 || u.Query().Get("X-Goog-Signature") == "" {
		t.Fatalf("not a V4 signed URL with this expiry: %s, expires %q", u.Path, u.Query().Get("X-Goog-Expires"))
	}
	if strings.Contains(link, "PRIVATE") {
		t.Fatal("the key leaked into the link")
	}
	if _, err := s.TemporaryLink(ctx, "missing", time.Hour); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("link to a missing object: %v", err)
	}
}

func TestTemporaryLinkWithoutAKeyIsDeclared(t *testing.T) {
	srv := inProcessServer(t)
	srv.createBucket(t, "videos-standard")
	s := openGCS(t, srv, "acme", domain.RoleProxy, config.StorageInstance{Prefix: "links/"})
	if _, err := s.Put(context.Background(), "k", strings.NewReader("x"), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TemporaryLink(context.Background(), "k", time.Hour); !errs.IsCode(err, errs.CodeCapabilityUnsupported) {
		t.Fatalf("%v", err)
	}
}

func TestDirectAccessComesFromUsage(t *testing.T) {
	srv := inProcessServer(t)
	for usage, want := range map[config.StorageUsage]bool{
		config.UsageOperational: true, config.UsageBackupOnly: false, "": false,
	} {
		s := openGCS(t, srv, "acme", domain.RoleMaster, config.StorageInstance{Usage: usage})
		if s.Capabilities().DirectAccess != want {
			t.Errorf("usage %q: %+v", usage, s.Capabilities())
		}
	}
}
