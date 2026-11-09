package apiclient_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/apiclient/apitest"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// TestMain lets the test binary stand in for the system ssh client. No test here
// talks to a real SSH server.
func TestMain(m *testing.M) {
	switch os.Getenv("VIDYOM_FAKE_SSH") {
	case "forward":
		fakeSSHForward()
	case "fail":
		fmt.Fprintln(os.Stderr, "ssh: connect to host: Connection refused")
		os.Exit(255)
	}
	os.Exit(m.Run())
}

// fakeSSHForward implements the part of `ssh -N -L local:remote` that the client
// relies on.
func fakeSSHForward() {
	args := os.Args[1:]
	var forward string
	for i, arg := range args {
		if arg == "-L" && i+1 < len(args) {
			forward = args[i+1]
		}
	}
	local, remote, ok := strings.Cut(forward, ":")
	if !ok {
		os.Exit(2)
	}
	if len(args) < 2 || args[len(args)-2] != "--" {
		fmt.Fprintln(os.Stderr, "the target must follow --")
		os.Exit(2)
	}
	listener, err := net.Listen("unix", local)
	if err != nil {
		os.Exit(3)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			os.Exit(0)
		}
		go func() {
			defer conn.Close()
			upstream, err := net.Dial("unix", remote)
			if err != nil {
				return
			}
			defer upstream.Close()
			go func() { _, _ = io.Copy(upstream, conn) }()
			_, _ = io.Copy(conn, upstream)
		}()
	}
}

func sshClient(t *testing.T, socket string, tok apiclient.TokenFunc) *apiclient.Client {
	t.Helper()
	c, err := apiclient.New(apiclient.Options{
		Endpoint: apiclient.Endpoint{
			Kind: apiclient.KindSSH, SSHTarget: "operator@media.example.internal",
			RemoteSocket: socket, SSHBinary: os.Args[0],
		},
		Tenant: "acme",
		Token:  tok,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSSHTunnelCarriesRequestsAndCredential(t *testing.T) {
	t.Setenv("VIDYOM_FAKE_SSH", "forward")
	srv := apitest.New(t)
	srv.Token = testToken
	srv.Handle(api.RouteAssetsList, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteJSON(w, 200, map[string]any{"items": []any{}})
	})
	c := sshClient(t, srv.ServeUnix(), token(testToken, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Do(ctx, apiclient.Call{Route: api.RouteAssetsList}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, apiclient.Call{Route: api.RouteAssetsList}, nil); err != nil {
		t.Fatal("the tunnel must be reused:", err)
	}
}

func TestSSHRequiresPairing(t *testing.T) {
	t.Setenv("VIDYOM_FAKE_SSH", "forward")
	srv := apitest.New(t)
	c := sshClient(t, srv.ServeUnix(), nil)
	err := c.Do(context.Background(), apiclient.Call{Route: api.RouteAssetsList}, nil)
	if !errs.IsCode(err, errs.CodeUnauthenticated) {
		t.Fatalf("D12 keeps pairing mandatory over SSH: err = %v", err)
	}
	if len(srv.Calls()) != 0 {
		t.Fatal("nothing may be sent")
	}
}

func TestSSHFailureReportsTheClientError(t *testing.T) {
	t.Setenv("VIDYOM_FAKE_SSH", "fail")
	srv := apitest.New(t)
	c := sshClient(t, srv.ServeUnix(), token(testToken, nil))
	err := c.Do(context.Background(), apiclient.Call{Route: api.RouteHealth}, nil)
	if !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestSSHTargetCannotBeAnOption(t *testing.T) {
	for _, target := range []string{"-oProxyCommand=evil", "-L", "", "user@", "@host", "a b@c", "user@-host"} {
		_, err := apiclient.New(apiclient.Options{Endpoint: apiclient.Endpoint{
			Kind: apiclient.KindSSH, SSHTarget: target, RemoteSocket: "/run/vidyom/vidyomd.sock",
		}})
		if !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%q: err = %v", target, err)
		}
	}
	_, err := apiclient.New(apiclient.Options{Endpoint: apiclient.Endpoint{
		Kind: apiclient.KindSSH, SSHTarget: "user@host", RemoteSocket: "relative.sock",
	}})
	if !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("relative socket: err = %v", err)
	}
}
