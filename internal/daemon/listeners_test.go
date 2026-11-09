package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

// ---- Master key (D13, §15.6) ----

func TestNoListenerWhenSealed(t *testing.T) {
	cases := map[string]func(*Deps){
		"provider fails":   func(d *Deps) { d.MasterKey = &fakeProvider{err: errors.New("sealed")} },
		"no provider":      func(d *Deps) { d.MasterKey = nil },
		"key is too short": func(d *Deps) { d.MasterKey = &fakeProvider{key: make([]byte, 16)} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig(t)
			deps := testDeps(&fakeAuthn{})
			mutate(&deps)
			mustNotStart(t, cfg, deps)
			socketGone(t, cfg.Listeners.Unix.Path)
		})
	}
}

func TestNoProviderInConfigurationRefuses(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Vault.Unseal.Provider = ""
	err := mustNotStart(t, cfg, testDeps(&fakeAuthn{}))
	if !strings.Contains(err.Error(), "master key") {
		t.Fatalf("error = %v", err)
	}
	socketGone(t, cfg.Listeners.Unix.Path)
}

func TestSealedDaemonLeavesNoHTTPSListener(t *testing.T) {
	cfg := baseConfig(t)
	port := freePort(t)
	cfg.Listeners = config.Listeners{HTTPS: &config.HTTPSListener{Bind: "127.0.0.1", Port: port, TLS: config.TLS{Mode: config.TLSAuto}}}
	deps := testDeps(&fakeAuthn{})
	deps.MasterKey = &fakeProvider{err: errors.New("sealed")}
	mustNotStart(t, cfg, deps)
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("something is listening")
	}
}

func TestRequiredDependencies(t *testing.T) {
	for name, mutate := range map[string]func(*Deps){
		"authenticator": func(d *Deps) { d.API.Authenticator = nil },
		"authorizer":    func(d *Deps) { d.API.Authorizer = nil },
		"auditor":       func(d *Deps) { d.API.Auditor = nil },
		"stores":        func(d *Deps) { d.API.TenantStores = nil },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig(t)
			deps := testDeps(&fakeAuthn{})
			mutate(&deps)
			mustNotStart(t, cfg, deps)
			socketGone(t, cfg.Listeners.Unix.Path)
		})
	}
}

func TestBootRecord(t *testing.T) {
	cfg := baseConfig(t)
	deps := testDeps(&fakeAuthn{})
	audit := deps.API.Auditor.(*fakeAuditor)
	r := start(t, cfg, deps)
	defer r.stop(t)
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if len(audit.entries) != 1 || audit.entries[0].Action != "daemon.boot" || audit.entries[0].TenantID != "" {
		t.Fatalf("entries = %+v", audit.entries)
	}
	if audit.entries[0].Details["sealing"] != "os" {
		t.Fatalf("details = %+v", audit.entries[0].Details)
	}
}

func TestBrokenAuditStopsBoot(t *testing.T) {
	cfg := baseConfig(t)
	deps := testDeps(&fakeAuthn{})
	deps.API.Auditor.(*fakeAuditor).err = errors.New("disk full")
	mustNotStart(t, cfg, deps)
	socketGone(t, cfg.Listeners.Unix.Path)
}

// ---- Interface binding (§9.3, §15.6) ----

func TestMissingInterfaceAbortsWithNothingListening(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Listeners.HTTPS = &config.HTTPSListener{
		Interface: "vidyom-dev-api-missing0", Port: freePort(t),
		TLS: config.TLS{Mode: config.TLSFiles, CertFile: "/nope", KeyFile: "/nope"},
	}
	err := mustNotStart(t, cfg, testDeps(&fakeAuthn{}))
	if !strings.Contains(err.Error(), "vidyom-dev-api-missing0") {
		t.Fatalf("error = %v", err)
	}
	// Nothing fell back: not the socket either, and nothing on the port.
	socketGone(t, cfg.Listeners.Unix.Path)
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Listeners.HTTPS.Port), 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("something is listening")
	}
}

func TestInterfaceDownOrEmptyAborts(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Listeners = config.Listeners{HTTPS: &config.HTTPSListener{Interface: "tailscale0", Port: 0, TLS: config.TLS{Mode: config.TLSAuto}}}
	deps := testDeps(&fakeAuthn{})
	deps.interfaces = func(string) ([]net.IP, error) { return nil, errors.New("the interface is down") }
	err := mustNotStart(t, cfg, deps)
	if !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("error = %v", err)
	}
}

func TestRealInterfaceLookup(t *testing.T) {
	if _, err := interfaceAddresses("vidyom-dev-api-missing0"); err == nil {
		t.Fatal("a missing interface resolved")
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback == 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		ips, err := interfaceAddresses(iface.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, ip := range ips {
			if ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				t.Fatalf("unusable address %s", ip)
			}
		}
		if len(ips) == 0 {
			t.Fatal("no address on loopback")
		}
		return
	}
	t.Skip("no loopback interface")
}

func TestInterfaceBindsEveryAddressAndNothingElse(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Listeners = config.Listeners{HTTPS: &config.HTTPSListener{Interface: "ovl0", Port: freePort(t), TLS: config.TLS{Mode: config.TLSAuto}}}
	deps := testDeps(&fakeAuthn{})
	deps.interfaces = func(name string) ([]net.IP, error) {
		if name != "ovl0" {
			t.Errorf("looked up %q", name)
		}
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	r := start(t, cfg, deps)
	defer r.stop(t)
	if len(r.infos) != 1 || !strings.HasPrefix(r.infos[0].Address, "127.0.0.1:") || r.infos[0].Interface != "ovl0" {
		t.Fatalf("infos = %+v", r.infos)
	}
}

func TestContainerPortRule(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Listeners = config.Listeners{HTTPS: &config.HTTPSListener{Interface: "eth0", Port: 0, LoopbackPublished: true, TLS: config.TLS{Mode: config.TLSAuto}}}
	deps := testDeps(&fakeAuthn{})

	// A public address on the "container" interface is refused even with the flag.
	deps.interfaces = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.5")}, nil }
	mustNotStart(t, cfg, deps)

	// Without the flag a private address may not use an automatic certificate.
	cfg.Listeners.HTTPS.LoopbackPublished = false
	deps.interfaces = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("172.20.0.5")}, nil }
	mustNotStart(t, cfg, deps)

	// With the flag the plan is accepted, and marked for doctor. Binding 172.20.0.5
	// needs that address on this host, so this stops at the plan.
	cfg.Listeners.HTTPS.LoopbackPublished = true
	plans, fingerprint, err := planListeners(cfg, deps.interfaces)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !plans[0].info.LoopbackPublished || fingerprint == "" {
		t.Fatalf("plans = %+v, %q", plans, fingerprint)
	}
}

// ---- All or nothing (I19, I134) ----

func TestOneFailedListenerAbortsAll(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	cfg := baseConfig(t)
	cfg.Listeners.HTTPS = &config.HTTPSListener{Bind: "127.0.0.1", Port: occupied.Addr().(*net.TCPAddr).Port, TLS: config.TLS{Mode: config.TLSAuto}}
	mustNotStart(t, cfg, testDeps(&fakeAuthn{}))
	socketGone(t, cfg.Listeners.Unix.Path)
}

func TestBadCertificateAbortsBeforeAnyBind(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Listeners.HTTPS = &config.HTTPSListener{
		Bind: "127.0.0.1", Port: 0,
		TLS: config.TLS{Mode: config.TLSFiles, CertFile: filepath.Join(cfg.DataDir, "missing.crt"), KeyFile: filepath.Join(cfg.DataDir, "missing.key")},
	}
	mustNotStart(t, cfg, testDeps(&fakeAuthn{}))
	socketGone(t, cfg.Listeners.Unix.Path)
}

func TestPreflightRunsBeforeListeners(t *testing.T) {
	cfg := baseConfig(t)
	deps := testDeps(&fakeAuthn{})
	deps.Preflight = []func(context.Context) error{
		func(context.Context) error {
			if _, err := os.Lstat(cfg.Listeners.Unix.Path); err == nil {
				t.Error("a socket exists during preflight")
			}
			return errors.New("migration failed")
		},
	}
	err := mustNotStart(t, cfg, deps)
	if !strings.Contains(err.Error(), "migration failed") {
		t.Fatalf("error = %v", err)
	}
	socketGone(t, cfg.Listeners.Unix.Path)
}

// ---- Unix socket ----

func TestUnixSocketModeAndPeerIdentity(t *testing.T) {
	cfg := baseConfig(t)
	authn := &fakeAuthn{}
	r := start(t, cfg, testDeps(authn))
	path := cfg.Listeners.Unix.Path

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o", perm)
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := parent.Mode().Perm(); perm != 0o700 {
		t.Errorf("parent mode = %o", perm)
	}
	if len(r.infos) != 1 || r.infos[0].Kind != "unix" || r.infos[0].Mode != "0600" {
		t.Errorf("infos = %+v", r.infos)
	}

	// No token: the kernel says who is connecting.
	client := unixClient(path)
	resp, err := client.Get("http://vidyomd/api/v1/tenants")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	// Tenants is unwired here, so 501 proves the request got past authentication.
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	authn.mu.Lock()
	uids := append([]int(nil), authn.peerUIDs...)
	authn.mu.Unlock()
	if len(uids) != 1 || uids[0] != os.Getuid() {
		t.Fatalf("peer UIDs = %v, want %d", uids, os.Getuid())
	}

	// Stopping removes the socket and returns no error.
	if err := r.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	socketGone(t, path)
}

func TestSocketCustomMode(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Listeners.Unix.Mode = "0660"
	r := start(t, cfg, testDeps(&fakeAuthn{}))
	defer r.stop(t)
	info, err := os.Stat(cfg.Listeners.Unix.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o660 {
		t.Fatalf("mode = %o", perm)
	}
}

func TestSocketPathRules(t *testing.T) {
	t.Run("parent writable by others", func(t *testing.T) {
		dir := filepath.Join(tempDir(t), "open")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		err := prepareSocketPath(filepath.Join(dir, "d.sock"))
		if err == nil || !strings.Contains(err.Error(), "writable by others") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("regular file at the path", func(t *testing.T) {
		path := filepath.Join(tempDir(t), "d.sock")
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := prepareSocketPath(path); err == nil {
			t.Fatal("accepted")
		}
		if data, _ := os.ReadFile(path); string(data) != "data" {
			t.Fatal("the file was touched")
		}
	})
	t.Run("symlink at the path", func(t *testing.T) {
		dir := tempDir(t)
		path := filepath.Join(dir, "d.sock")
		if err := os.Symlink(filepath.Join(dir, "elsewhere"), path); err != nil {
			t.Fatal(err)
		}
		if err := prepareSocketPath(path); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("live socket", func(t *testing.T) {
		path := filepath.Join(tempDir(t), "d.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		if err := prepareSocketPath(path); err == nil || !strings.Contains(err.Error(), "another daemon") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("stale socket is replaced", func(t *testing.T) {
		path := filepath.Join(tempDir(t), "d.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = ln.Close()
		if err := prepareSocketPath(path); err != nil {
			t.Fatal(err)
		}
		socketGone(t, path)
	})
	t.Run("parent is created private", func(t *testing.T) {
		path := filepath.Join(tempDir(t), "a", "b", "d.sock")
		if err := prepareSocketPath(path); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Dir(path))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("mode: %v, %v", info, err)
		}
	})
}

func TestSecondDaemonIsRefused(t *testing.T) {
	cfg := baseConfig(t)
	r := start(t, cfg, testDeps(&fakeAuthn{}))
	defer r.stop(t)

	second := cfg
	second.Listeners = config.Listeners{Unix: &config.UnixListener{Path: filepath.Join(tempDir(t), "other.sock")}}
	err := mustNotStart(t, second, testDeps(&fakeAuthn{}))
	if !strings.Contains(err.Error(), "another vidyomd") {
		t.Fatalf("error = %v", err)
	}
	socketGone(t, second.Listeners.Unix.Path)
}

func TestLockReleasedOnExit(t *testing.T) {
	cfg := baseConfig(t)
	r := start(t, cfg, testDeps(&fakeAuthn{}))
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	r = start(t, cfg, testDeps(&fakeAuthn{}))
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
}

func TestPIDFile(t *testing.T) {
	cfg := baseConfig(t)
	r := start(t, cfg, testDeps(&fakeAuthn{}))
	path := filepath.Join(cfg.DataDir, "vidyomd.pid")
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) != fmt.Sprint(os.Getpid()) {
		t.Fatalf("pid file: %q, %v", data, err)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pid file kept")
	}
}

// ---- HTTPS (§9.2, D12, D49) ----

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// pinnedClient trusts exactly one certificate, the way the CLI pins (I164).
func pinnedClient(fingerprint string, seen *string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS13,
				InsecureSkipVerify: true,
				VerifyConnection: func(cs tls.ConnectionState) error {
					got := Fingerprint(cs.PeerCertificates[0].Raw)
					if seen != nil {
						*seen = got
					}
					if fingerprint != "" && got != fingerprint {
						return errors.New("pin mismatch")
					}
					return nil
				},
			},
		},
	}
}

func httpsConfig(t *testing.T) config.Daemon {
	t.Helper()
	cfg := baseConfig(t)
	cfg.Listeners = config.Listeners{HTTPS: &config.HTTPSListener{Bind: "127.0.0.1", Port: 0, TLS: config.TLS{Mode: config.TLSAuto}}}
	return cfg
}

func TestHTTPSOnLoopbackWithPin(t *testing.T) {
	cfg := httpsConfig(t)
	r := start(t, cfg, testDeps(&fakeAuthn{}))
	addr := r.infos[0].Address

	// The served certificate is the one the data directory holds.
	var seen string
	resp, err := pinnedClient("", &seen).Get("https://" + addr + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	var health api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || health.Sealing != "os" {
		t.Fatalf("health: %d %+v", resp.StatusCode, health)
	}
	if resp.TLS == nil || resp.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("TLS: %+v", resp.TLS)
	}
	if !strings.HasPrefix(seen, "sha256:") || len(seen) != len("sha256:")+64 || strings.ToLower(seen) != seen {
		t.Fatalf("fingerprint form: %q", seen)
	}

	// The pin survives a restart.
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	r = start(t, cfg, testDeps(&fakeAuthn{}))
	defer r.stop(t)
	resp, err = pinnedClient(seen, nil).Get("https://" + r.infos[0].Address + "/api/v1/version")
	if err != nil {
		t.Fatalf("the certificate changed across a restart: %v", err)
	}
	_ = resp.Body.Close()

	// A wrong pin does not connect.
	if _, err := pinnedClient("sha256:0000", nil).Get("https://" + r.infos[0].Address + "/api/v1/version"); err == nil {
		t.Fatal("a wrong pin connected")
	}

	// The key is private.
	info, err := os.Stat(filepath.Join(cfg.DataDir, "tls", "server.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode: %v, %v", info, err)
	}
}

func TestHTTPSRefusesOldTLSAndPlaintext(t *testing.T) {
	cfg := httpsConfig(t)
	r := start(t, cfg, testDeps(&fakeAuthn{}))
	defer r.stop(t)
	addr := r.infos[0].Address

	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12})
	if err == nil {
		_ = conn.Close()
		t.Fatal("TLS 1.2 connected")
	}

	// Plain HTTP gets no API answer.
	raw, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(raw, "GET /api/v1/health HTTP/1.0\r\n\r\n")
	line, _ := bufio.NewReader(raw).ReadString('\n')
	if strings.Contains(line, " 200 ") {
		t.Fatalf("plaintext answered: %q", line)
	}
}

func writeTestCert(t *testing.T, dir, name string, isCA bool, parent *x509.Certificate, parentKey *rsa.PrivateKey) (string, string, *x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:         isCA, BasicConstraintsValid: true,
	}
	issuer, signer := template, key
	if parent != nil {
		issuer, signer = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, name+".crt")
	keyPath := filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, cert, key
}

func TestHTTPSWithFilesAndMutualTLS(t *testing.T) {
	dir := tempDir(t)
	caPath, _, caCert, caKey := writeTestCert(t, dir, "ca", true, nil, nil)
	serverPath, serverKey, _, _ := writeTestCert(t, dir, "server", false, caCert, caKey)
	clientPath, clientKey, _, _ := writeTestCert(t, dir, "client", false, caCert, caKey)

	cfg := baseConfig(t)
	cfg.Listeners = config.Listeners{HTTPS: &config.HTTPSListener{
		Bind: "127.0.0.1", Port: 0,
		TLS: config.TLS{Mode: config.TLSFiles, CertFile: serverPath, KeyFile: serverKey, ClientCAFile: caPath},
	}}
	r := start(t, cfg, testDeps(&fakeAuthn{}))
	defer r.stop(t)
	addr := r.infos[0].Address
	if !r.infos[0].MutualTLS {
		t.Error("mutual TLS not reported")
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	withCert, err := tls.LoadX509KeyPair(clientPath, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	client := func(certs ...tls.Certificate) *http.Client {
		return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: certs},
		}}
	}
	if resp, err := client().Get("https://" + addr + "/api/v1/health"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("no client certificate was accepted")
	}
	resp, err := client(withCert).Get("https://" + addr + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// No fingerprint is advertised for an operator's certificate.
	plans, fingerprint, err := planListeners(cfg, interfaceAddresses)
	if err != nil || fingerprint != "" || len(plans) != 1 {
		t.Fatalf("plans: %v %q %v", plans, fingerprint, err)
	}
}

type pairingDevices struct{ service.Devices }

func (pairingDevices) CreatePairingCode(context.Context, service.Scope, api.CreatePairingCodeRequest) (api.PairingCodeResponse, error) {
	return api.PairingCodeResponse{Code: "ABCDE"}, nil
}

func TestPairingCodeCarriesThePin(t *testing.T) {
	cfg := httpsConfig(t)
	deps := testDeps(&fakeAuthn{})
	deps.API.Devices = pairingDevices{}
	r := start(t, cfg, deps)
	defer r.stop(t)

	var seen string
	client := pinnedClient("", &seen)
	req, err := http.NewRequest("POST", "https://"+r.infos[0].Address+"/api/v1/pairing-codes", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer good-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body api.PairingCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.TLSFingerprint == "" || body.TLSFingerprint != seen {
		t.Fatalf("response %q, served %q", body.TLSFingerprint, seen)
	}
}

func TestExpiredAutoCertIsReplaced(t *testing.T) {
	dir := tempDir(t)
	if _, first, err := ensureAutoCert(dir, nil); err != nil {
		t.Fatal(err)
	} else {
		// Reuse while valid.
		if _, again, err := ensureAutoCert(dir, nil); err != nil || again != first {
			t.Fatalf("reuse: %q %v", again, err)
		}
		// Corrupt: replaced, and the pin changes, which clients see as a mismatch.
		if err := os.WriteFile(filepath.Join(dir, "tls", "server.crt"), []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, replaced, err := ensureAutoCert(dir, nil); err != nil || replaced == first {
			t.Fatalf("replace: %q %v", replaced, err)
		}
	}
}

func TestAutoCertNames(t *testing.T) {
	cert, _, err := ensureAutoCert(tempDir(t), []net.IP{net.ParseIP("172.20.0.5")})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"127.0.0.1", "::1", "172.20.0.5"} {
		found := false
		for _, ip := range leaf.IPAddresses {
			if ip.Equal(net.ParseIP(want)) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not in the certificate: %v", want, leaf.IPAddresses)
		}
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "localhost" {
		t.Errorf("names: %v", leaf.DNSNames)
	}
	sum := sha256.Sum256(cert.Certificate[0])
	if Fingerprint(cert.Certificate[0]) != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Error("fingerprint is not the SHA-256 of the leaf's DER")
	}
}

// ---- Lifecycle ----

func TestBackgroundServices(t *testing.T) {
	cfg := baseConfig(t)
	deps := testDeps(&fakeAuthn{})
	running := make(chan struct{})
	stopped := make(chan struct{})
	deps.Background = []BackgroundService{{Name: "engine", Run: func(ctx context.Context) error {
		close(running)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}}}
	r := start(t, cfg, deps)
	<-running
	if err := r.stop(t); err != nil {
		t.Fatalf("a clean stop reported: %v", err)
	}
	<-stopped
}

func TestBackgroundFailureStopsTheDaemon(t *testing.T) {
	cfg := baseConfig(t)
	deps := testDeps(&fakeAuthn{})
	deps.Background = []BackgroundService{{Name: "engine", Run: func(context.Context) error {
		return errors.New("lost the database")
	}}}
	ready := make(chan []ListenerInfo, 1)
	deps.OnReady = func(infos []ListenerInfo) { ready <- infos }
	err := Run(context.Background(), cfg, deps)
	if err == nil || !strings.Contains(err.Error(), "lost the database") {
		t.Fatalf("error = %v", err)
	}
	socketGone(t, cfg.Listeners.Unix.Path)
}

type slowJobs struct{ service.Jobs }

func (slowJobs) Subscribe(ctx context.Context, _ service.Scope, _ id.Ref, _ int64) (<-chan domain.JobEvent, error) {
	return make(chan domain.JobEvent), nil
}

func TestOpenStreamDoesNotHoldUpShutdown(t *testing.T) {
	cfg := baseConfig(t)
	deps := testDeps(&fakeAuthn{})
	deps.API.Jobs = slowJobs{}
	r := start(t, cfg, deps)

	client := unixClient(cfg.Listeners.Unix.Path)
	client.Timeout = 0
	req, err := http.NewRequest("GET", "http://vidyomd/api/v1/jobs/JOB-001/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(api.HeaderTenant, "acme")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	began := time.Now()
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("shutdown took %s with a stream open", took)
	}
}

func TestReload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGHUP")
	}
	cfg := baseConfig(t)
	deps := testDeps(&fakeAuthn{})
	reloaded := make(chan struct{}, 1)
	deps.Reload = func(context.Context) error {
		reloaded <- struct{}{}
		return nil
	}
	r := start(t, cfg, deps)
	defer r.stop(t)
	if err := sendHangup(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloaded:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not reload")
	}
}

func TestDefaultsAreInstalled(t *testing.T) {
	cfg := baseConfig(t)
	cfg.IngestRoots = []string{tempDir(t)}
	deps := testDeps(&fakeAuthn{})
	r := start(t, cfg, deps)
	defer r.stop(t)

	// The doctor and the upload service are this component's own.
	client := unixClient(cfg.Listeners.Unix.Path)
	resp, err := client.Get("http://vidyomd/api/v1/doctor")
	if err != nil {
		t.Fatal(err)
	}
	var doctorBody api.DoctorResponse
	if err := json.NewDecoder(resp.Body).Decode(&doctorBody); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(doctorBody.Checks) == 0 {
		t.Fatalf("doctor: %d %+v", resp.StatusCode, doctorBody)
	}

	req, err := http.NewRequest("POST", "http://vidyomd/api/v1/assets/uploads", strings.NewReader(
		`{"filename":"a.mp4","size_bytes":1,"sha256":"`+strings.Repeat("a", 64)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(api.HeaderTenant, "acme")
	req.Header.Set(api.HeaderIdempotencyKey, "k1")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.SpoolDir, "acme"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("spool: %v, %v", entries, err)
	}
}
