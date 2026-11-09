// Command probe is a test-only plugin that tries what a plugin must not be able to do,
// and misbehaves on request. It reports what happened instead of deciding anything.
//
// Its arguments choose how it starts, which is the point of those tests:
// --no-init never answers initialize, --wrong-id and --wrong-protocol answer falsely,
// --claim-more claims more than a manifest says, --crash-at-start exits,
// --noisy-stdout writes garbage before answering, and --ignore-shutdown ignores shutdown,
// a closed stdin and SIGTERM.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/jsonrpc"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
)

var (
	conn     *jsonrpc.Conn
	tempDir  string
	tenantID string
	args     = os.Args[1:]
)

func has(flag string) bool { return slices.Contains(args, flag) }

type request struct {
	Path    string `json:"path"`
	Data    string `json:"data"`
	Addr    string `json:"addr"`
	URL     string `json:"url"`
	Name    string `json:"name"`
	Millis  int    `json:"millis"`
	Count   int    `json:"count"`
	Command string `json:"command"`
	Text    string `json:"text"`
}

func main() {
	if has("--crash-at-start") {
		fmt.Fprintln(os.Stderr, "crashing as asked")
		os.Exit(3)
	}
	if has("--noisy-stdout") {
		fmt.Fprintln(os.Stdout, "this is not JSON")
	}
	if has("--ignore-shutdown") {
		signal.Ignore(syscall.SIGTERM)
	}
	conn = jsonrpc.NewConn(os.Stdin, os.Stdout, nil, handle)
	_ = conn.Run(context.Background())
	if has("--ignore-shutdown") {
		select {} // stdin closed and SIGTERM ignored: only SIGKILL ends this
	}
}

func handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case plugin.MethodInitialize:
		return initialize(params)
	case plugin.MethodShutdown:
		if has("--ignore-shutdown") {
			select {} // never returns, never exits on its own
		}
		return struct{}{}, nil
	case plugin.MethodHookInvoke:
		var inv plugin.HookInvokeParams
		if err := json.Unmarshal(params, &inv); err != nil {
			return nil, err
		}
		return invoke(ctx, inv)
	case plugin.MethodEventDeliver:
		var e plugin.EventDeliverParams
		_ = json.Unmarshal(params, &e)
		if string(e.Payload) != "" {
			fmt.Fprintln(os.Stderr, "event payload", string(e.Payload))
		}
		return struct{}{}, nil
	}
	return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not found"}
}

func initialize(params json.RawMessage) (any, error) {
	if has("--no-init") {
		select {}
	}
	var p plugin.InitializeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	tempDir = p.TempDir
	tenantID = string(p.TenantID)
	result := plugin.InitializeResult{ProtocolVersion: plugin.ProtocolVersion, PluginID: os.Getenv("VIDYOM_PLUGIN_ID")}
	if has("--wrong-id") {
		result.PluginID = "someone-else"
	}
	if has("--wrong-protocol") {
		result.ProtocolVersion = 99
	}
	if has("--claim-more") {
		result.Provides.NodeTypes = []string{"evil.node"}
	}
	return result, nil
}

func answer(allow bool, text string) hook.Result { return hook.Result{Allow: allow, Reason: text} }

func invoke(ctx context.Context, inv hook.Invocation) (any, error) {
	var req request
	if len(inv.Data) > 0 {
		if err := json.Unmarshal(inv.Data, &req); err != nil {
			return nil, err
		}
	}
	switch inv.Hook.Action {
	case "env":
		data, _ := json.Marshal(os.Environ())
		return hook.Result{Allow: true, Parameters: data}, nil
	case "whoami":
		data, _ := json.Marshal(map[string]any{"tenant": tenantID, "pid": os.Getpid(), "temp": tempDir, "key": inv.IdempotencyKey, "trace": inv.TraceID})
		return hook.Result{Allow: true, Parameters: data}, nil
	case "echo-data":
		return hook.Result{Allow: true, Parameters: inv.Data}, nil
	case "read-file":
		data, err := os.ReadFile(req.Path)
		if err != nil {
			return answer(false, err.Error()), nil
		}
		return answer(true, string(data)), nil
	case "write-file":
		if err := os.WriteFile(req.Path, []byte(req.Data), 0o600); err != nil {
			return answer(false, err.Error()), nil
		}
		return answer(true, "written"), nil
	case "exec":
		out, err := exec.CommandContext(ctx, req.Command, "-c", "echo ran").CombinedOutput()
		if err != nil {
			return answer(false, err.Error()), nil
		}
		return answer(true, strings.TrimSpace(string(out))), nil
	case "dial":
		c, err := net.DialTimeout("tcp", req.Addr, 2*time.Second)
		if err != nil {
			return answer(false, err.Error()), nil
		}
		c.Close()
		return answer(true, "connected"), nil
	case "udp":
		c, err := net.Dial("udp", req.Addr)
		if err != nil {
			return answer(false, err.Error()), nil
		}
		c.Close()
		return answer(true, "connected"), nil
	case "unix":
		c, err := net.Dial("unix", req.Addr)
		if err != nil {
			return answer(false, err.Error()), nil
		}
		c.Close()
		return answer(true, "connected"), nil
	case "http-get":
		// Explicit, because Go's environment proxy skips loopback destinations, and the
		// tests' destinations are on loopback.
		client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
			if p := os.Getenv("HTTP_PROXY"); p != "" {
				return url.Parse(p)
			}
			return nil, nil
		}}}
		resp, err := client.Get(req.URL)
		if err != nil {
			return answer(false, err.Error()), nil
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return answer(resp.StatusCode < 300, strconv.Itoa(resp.StatusCode)+" "+string(body)), nil
	case "keyctl":
		// keyctl(KEYCTL_GET_KEYRING_ID, KEY_SPEC_USER_KEYRING, 0): the kernel keyring.
		_, _, errno := syscall.Syscall(keyctlSyscall(), 0, ^uintptr(3), 0)
		if errno == syscall.EPERM {
			return answer(false, "denied"), nil
		}
		return answer(true, errno.Error()), nil
	case "secret":
		var result plugin.HostSecretResult
		if err := conn.Call(ctx, plugin.MethodHostSecret, plugin.HostSecretParams{Name: req.Name}, &result); err != nil {
			return answer(false, err.Error()), nil
		}
		return answer(true, result.Value), nil
	case "leak-secret":
		var result plugin.HostSecretResult
		if err := conn.Call(ctx, plugin.MethodHostSecret, plugin.HostSecretParams{Name: req.Name}, &result); err != nil {
			return nil, err
		}
		fmt.Fprintln(os.Stderr, "stderr has", result.Value)
		_ = conn.Notify(ctx, plugin.MethodHostLog, plugin.HostLogParams{Level: "warn", Event: "probe.leak", Message: "log has " + result.Value, Fields: map[string]any{"value": result.Value, "password": "hunter2"}})
		time.Sleep(50 * time.Millisecond)
		return answer(false, "reason has "+result.Value), nil
	case "fail-with-secret":
		var result plugin.HostSecretResult
		if err := conn.Call(ctx, plugin.MethodHostSecret, plugin.HostSecretParams{Name: req.Name}, &result); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("failed with %s", result.Value)
	case "sleep":
		select {
		case <-time.After(time.Duration(req.Millis) * time.Millisecond):
		case <-ctx.Done():
		}
		return answer(true, "slept"), nil
	case "ignore-cancel":
		time.Sleep(time.Duration(req.Millis) * time.Millisecond)
		return answer(true, "slept"), nil
	case "crash":
		os.Exit(7)
	case "oversized":
		// One line over the protocol's limit: the runtime must end the process.
		_, _ = os.Stdout.Write([]byte(strings.Repeat("x", jsonrpc.MaxMessageBytes+1024) + "\n"))
		return answer(true, "unreachable"), nil
	case "flood":
		for range req.Count {
			_ = conn.Notify(ctx, plugin.MethodHostLog, plugin.HostLogParams{Message: "flood"})
		}
		return answer(true, "flooded"), nil
	case "big-parameters":
		data, _ := json.Marshal(strings.Repeat("x", req.Count))
		return hook.Result{Allow: true, Parameters: data}, nil
	case "long-reason":
		return answer(true, strings.Repeat("r", req.Count)), nil
	case "spawn-child":
		// Keeps running after this process ends, unless the group is killed.
		cmd := exec.Command(os.Args[0], "--sleeper")
		if err := cmd.Start(); err != nil {
			return answer(false, err.Error()), nil
		}
		return answer(true, strconv.Itoa(cmd.Process.Pid)), nil
	case "stderr-only":
		fmt.Fprintln(os.Stderr, req.Text)
		return answer(true, "printed"), nil
	}
	return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "unknown action"}
}
