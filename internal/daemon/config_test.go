package daemon

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/config"
)

func TestResolveConfigDir(t *testing.T) {
	t.Setenv(ConfigDirEnv, "")
	if _, err := ResolveConfigDir(""); err == nil {
		t.Fatal("an implicit configuration directory was accepted")
	}
	t.Setenv(ConfigDirEnv, "/etc/vidyom-from-env")
	got, err := ResolveConfigDir("")
	if err != nil || got != "/etc/vidyom-from-env" {
		t.Fatalf("env: %q, %v", got, err)
	}
	got, err = ResolveConfigDir("/etc/vidyom-from-flag")
	if err != nil || got != "/etc/vidyom-from-flag" {
		t.Fatalf("flag: %q, %v", got, err)
	}
}

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadConfig(t *testing.T) {
	dir := writeConfig(t, `{"dataDir":"/var/lib/vidyom","redis":{"url":"redis://r:6379"},
		"vault":{"unseal":{"provider":"file","path":"/run/secrets/k","acknowledgeFileSealed":true}},
		"listeners":{"unix":{"path":"/run/vidyom/vidyomd.sock"}},
		"ingestRoots":["/data/ingest"]}`)
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigDir != dir {
		t.Errorf("ConfigDir = %q", cfg.ConfigDir)
	}
	if cfg.SpoolDir != "/var/lib/vidyom/spool" || cfg.ScratchDir != "/var/lib/vidyom/scratch" || cfg.CacheDir != "/var/lib/vidyom/cache" {
		t.Errorf("defaults: %+v", cfg)
	}
	if cfg.Listeners.HTTPS != nil {
		t.Error("an HTTPS listener appeared from nowhere")
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestLoadConfigFailsClosed(t *testing.T) {
	for name, text := range map[string]string{
		"misspelled key": `{"dataDir":"/d","listners":{}}`,
		"nested typo":    `{"dataDir":"/d","listeners":{"https":{"bind":"127.0.0.1","port":1,"allowPublc":true,"tls":{"mode":"auto"}}}}`,
		"trailing data":  `{"dataDir":"/d"} {"dataDir":"/e"}`,
		"not json":       `dataDir: /d`,
		"no data dir":    `{}`,
		"relative":       `{"dataDir":"data"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(writeConfig(t, text)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := LoadConfig(t.TempDir()); err == nil {
		t.Fatal("a missing file was accepted")
	}
}

func TestDefaultListenerIsTheSocketOnly(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no default listener on this platform")
	}
	cfg := config.Daemon{DataDir: "/d"}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Listeners.Unix == nil || cfg.Listeners.HTTPS != nil {
		t.Fatalf("listeners = %+v", cfg.Listeners)
	}
	// An HTTPS-only configuration gets no socket on the side.
	cfg = config.Daemon{DataDir: "/d", Listeners: config.Listeners{HTTPS: &config.HTTPSListener{Bind: "127.0.0.1", TLS: config.TLS{Mode: config.TLSAuto}}}}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Listeners.Unix != nil {
		t.Fatal("a socket appeared beside the HTTPS listener")
	}
}

func TestValidateConfigRefusals(t *testing.T) {
	good := func() config.Daemon {
		return config.Daemon{
			DataDir: "/d", SpoolDir: "/d/s", ScratchDir: "/d/t", CacheDir: "/d/c",
			Redis:     config.Redis{URL: "redis://r"},
			Vault:     config.Vault{Unseal: config.Unseal{Provider: config.UnsealFile}},
			Listeners: config.Listeners{Unix: &config.UnixListener{Path: "/run/v.sock"}},
		}
	}
	if err := ValidateConfig(good()); err != nil {
		t.Fatalf("good: %v", err)
	}
	for name, mutate := range map[string]func(*config.Daemon){
		"no provider":      func(c *config.Daemon) { c.Vault.Unseal.Provider = "" },
		"no redis":         func(c *config.Daemon) { c.Redis.URL = "" },
		"relative spool":   func(c *config.Daemon) { c.SpoolDir = "spool" },
		"relative root":    func(c *config.Daemon) { c.IngestRoots = []string{"data"} },
		"unnamed local":    func(c *config.Daemon) { c.LocalUsers = []config.LocalUser{{UID: 1}} },
		"negative uid":     func(c *config.Daemon) { c.LocalUsers = []config.LocalUser{{UID: -1, Email: "a@example.com"}} },
		"no listeners":     func(c *config.Daemon) { c.Listeners = config.Listeners{} },
		"relative socket":  func(c *config.Daemon) { c.Listeners.Unix.Path = "v.sock" },
		"long socket":      func(c *config.Daemon) { c.Listeners.Unix.Path = "/" + strings.Repeat("x", 200) },
		"open socket mode": func(c *config.Daemon) { c.Listeners.Unix.Mode = "0666" },
		"bad socket mode":  func(c *config.Daemon) { c.Listeners.Unix.Mode = "rw" },
		"owner cannot use": func(c *config.Daemon) { c.Listeners.Unix.Mode = "0060" },
	} {
		t.Run(name, func(t *testing.T) {
			c := good()
			mutate(&c)
			if err := ValidateConfig(c); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func https(mutate func(*config.HTTPSListener)) config.HTTPSListener {
	h := config.HTTPSListener{Bind: "127.0.0.1", Port: 8443, TLS: config.TLS{Mode: config.TLSAuto}}
	if mutate != nil {
		mutate(&h)
	}
	return h
}

func TestHTTPSRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.HTTPSListener)
		ok     bool
	}{
		{"loopback auto", nil, true},
		{"ipv6 loopback auto", func(h *config.HTTPSListener) { h.Bind = "::1" }, true},
		{"both bind and interface", func(h *config.HTTPSListener) { h.Interface = "tailscale0" }, false},
		{"neither", func(h *config.HTTPSListener) { h.Bind = "" }, false},
		{"host name", func(h *config.HTTPSListener) { h.Bind = "vidyom.example.internal" }, false},
		{"bad port", func(h *config.HTTPSListener) { h.Port = 70000 }, false},
		{"no tls mode", func(h *config.HTTPSListener) { h.TLS.Mode = "" }, false},
		{"unknown tls mode", func(h *config.HTTPSListener) { h.TLS.Mode = "none" }, false},
		{"files without paths", func(h *config.HTTPSListener) { h.TLS.Mode = config.TLSFiles }, false},
		{"auto with paths", func(h *config.HTTPSListener) { h.TLS.CertFile = "/c" }, false},

		// §9.3: never 0.0.0.0 by accident, and only with a real certificate.
		{"every interface alone", func(h *config.HTTPSListener) { h.Bind = "0.0.0.0" }, false},
		{"every interface v6", func(h *config.HTTPSListener) { h.Bind = "::" }, false},
		{"every interface with allowPublic, auto", func(h *config.HTTPSListener) { h.Bind = "0.0.0.0"; h.AllowPublic = true }, false},
		{"every interface with allowPublic, files", func(h *config.HTTPSListener) {
			h.Bind, h.AllowPublic = "0.0.0.0", true
			h.TLS = config.TLS{Mode: config.TLSFiles, CertFile: "/c", KeyFile: "/k"}
		}, true},

		// Automatic certificates: loopback only, or a declared container port (I133).
		{"private auto", func(h *config.HTTPSListener) { h.Bind = "192.168.1.10" }, false},
		{"public auto", func(h *config.HTTPSListener) { h.Bind = "203.0.113.5" }, false},
		{"private files", func(h *config.HTTPSListener) {
			h.Bind = "192.168.1.10"
			h.TLS = config.TLS{Mode: config.TLSFiles, CertFile: "/c", KeyFile: "/k"}
		}, true},
		{"loopback published private", func(h *config.HTTPSListener) { h.Bind, h.LoopbackPublished = "172.20.0.5", true }, true},
		{"loopback published public", func(h *config.HTTPSListener) { h.Bind, h.LoopbackPublished = "203.0.113.5", true }, false},
		{"loopback published everywhere", func(h *config.HTTPSListener) {
			h.Bind, h.LoopbackPublished, h.AllowPublic = "0.0.0.0", true, true
			h.TLS = config.TLS{Mode: config.TLSFiles, CertFile: "/c", KeyFile: "/k"}
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateHTTPS(https(c.mutate))
			if c.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestIsPrivate(t *testing.T) {
	for ip, want := range map[string]bool{
		"10.0.0.1": true, "172.16.0.1": true, "192.168.0.1": true, "fd7a:115c:a1e0::1": true,
		"100.64.0.1": true, "100.127.255.1": true, "127.0.0.1": true, "::1": true,
		"100.128.0.1": false, "8.8.8.8": false, "203.0.113.5": false, "2001:db8::1": false,
	} {
		if got := isPrivate(net.ParseIP(ip)); got != want {
			t.Errorf("%s: %v", ip, got)
		}
	}
}
