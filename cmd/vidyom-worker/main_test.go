package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// clearSeams empties the seams for a test, whatever wire.go set, and restores them.
func clearSeams(t *testing.T) {
	t.Helper()
	registries, queue := wireRegistries, wireQueue
	wireRegistries, wireQueue = nil, nil
	t.Cleanup(func() { wireRegistries, wireQueue = registries, queue })
}

func TestNotWiredExitsWithAClearError(t *testing.T) {
	clearSeams(t)
	var stderr bytes.Buffer
	code := run(nil, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "not wired") {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
}

func TestVersion(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"--version"}, &stderr); code != 0 || !strings.Contains(stderr.String(), "vidyom-worker") {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
}

func TestUnknownFlagAndArgument(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"--no-such-flag"}, &stderr); code != 2 {
		t.Fatalf("code %d", code)
	}
	if code := run([]string{"stray"}, &stderr); code != 2 {
		t.Fatalf("code %d", code)
	}
}

func TestPools(t *testing.T) {
	pools, err := parsePools("media, ai")
	if err != nil || len(pools) != 2 || pools[0] != node.PoolMedia || pools[1] != node.PoolAI {
		t.Fatalf("%v %v", pools, err)
	}
	for _, bad := range []string{"daemon", "engine", "", "gpu"} {
		if _, err := parsePools(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTokenComesOnlyFromAFile(t *testing.T) {
	t.Setenv(tokenFileEnv, "")
	if _, err := readToken(""); err == nil {
		t.Fatal("no token source was accepted")
	}
	token, err := readToken(writeFile(t, "fictional-token\n"))
	if err != nil || string(token) != "fictional-token" {
		t.Fatalf("%q %v", token, err)
	}
	t.Setenv(tokenFileEnv, writeFile(t, "from-env-file"))
	token, err = readToken("")
	if err != nil || string(token) != "from-env-file" {
		t.Fatalf("%q %v", token, err)
	}
	if _, err := readToken(writeFile(t, "  \n")); err == nil {
		t.Fatal("an empty token file was accepted")
	}
	if _, err := readToken(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing token file was accepted")
	}
}

func TestConfigFileAndFlags(t *testing.T) {
	cfg := config.Daemon{
		ScratchDir:  "/data/scratch",
		CacheDir:    "/data/cache",
		SpoolDir:    "/data/spool",
		IngestRoots: []string{"/data/in"},
		Listeners:   config.Listeners{Unix: &config.UnixListener{Path: "/run/vidyom/d.sock"}},
		AIWorker:    config.AIWorker{Endpoint: "unix:///run/vidyom/ai.sock"},
		Retention:   config.Retention{CacheIdleDays: 3, CacheMaxBytes: 1 << 30},
	}
	data, _ := json.Marshal(cfg)
	loaded, err := loadDaemonConfig(writeFile(t, string(data)))
	if err != nil {
		t.Fatal(err)
	}
	o, err := parseFlags([]string{"--scratch-dir", "/other/scratch", "--ingest-root", "/a", "--ingest-root", "/b", "--max-concurrency", "3"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	built, err := buildConfig(o, loaded, []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	if built.ScratchDir != "/other/scratch" || built.CacheDir != "/data/cache" || built.SpoolDir != "/data/spool" {
		t.Errorf("%+v", built)
	}
	if len(built.IngestRoots) != 2 || built.IngestRoots[0] != "/a" {
		t.Errorf("flags must replace the file's roots: %v", built.IngestRoots)
	}
	if built.AIWorker.Endpoint == "" || built.CacheIdle.Hours() != 72 || built.CacheMaxBytes != 1<<30 || built.Resources.MaxConcurrency != 3 {
		t.Errorf("%+v", built)
	}
	if _, err := buildConfig(options{pools: "media"}, config.Daemon{}, []byte("token")); err == nil {
		t.Error("no daemon address was accepted")
	}
	if _, err := loadDaemonConfig(writeFile(t, "{")); err == nil {
		t.Error("a broken configuration was accepted")
	}
}

func TestWiredPathFailsClosedOnMissingPieces(t *testing.T) {
	clearSeams(t)
	wireRegistries = func(node.Registrar, connector.Registrar) error { return nil }
	wireQueue = func(context.Context, config.Redis) (jobqueue.Queue, func(), error) {
		return nil, nil, errors.New("redis is down")
	}

	token := writeFile(t, "token")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no token", []string{"--token-file", filepath.Join(t.TempDir(), "absent")}, "worker token"},
		{"no daemon", []string{"--token-file", token}, "no daemon address"},
		{"plain http", []string{"--token-file", token, "--daemon", "http://media.example.internal"}, "unix:// or https://"},
		{"no redis", []string{"--token-file", token, "--daemon", "unix:///run/d.sock"}, "no Redis URL"},
		{"queue down", []string{"--token-file", token, "--daemon", "unix:///run/d.sock", "--redis-url", "redis://127.0.0.1:1"}, "redis is down"},
		{"bad level", []string{"--log-level", "loud"}, "invalid log level"},
		{"bad pool", []string{"--token-file", token, "--daemon", "unix:///run/d.sock", "--pools", "gpu"}, "unknown pool"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := run(c.args, &stderr)
			if code == 0 || !strings.Contains(stderr.String(), c.want) {
				t.Fatalf("code %d: %s", code, stderr.String())
			}
		})
	}
}
