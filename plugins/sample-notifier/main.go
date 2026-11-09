// Command sample-notifier is the sample plugin: a real child process speaking JSON-RPC
// 2.0 over stdin and stdout (D3, I42), used by the runtime's tests and meant as a
// starting point. It shows each thing a plugin can do in this version:
//
//   - check-source, for a before hook: blocks a source with no title (§15.7).
//   - record-finish, for a finish hook: writes to its private temp directory, and uses
//     the idempotency key so a retried hook counts once (§8.1).
//   - notify, for an after hook: asks the daemon for a granted secret and calls a
//     granted host through the proxy the runtime gives it.
//   - stats: reports what it has seen, for tests.
//   - event.deliver: receives publication.completed at least once, and uses the event ID
//     to count each event once (§8.2).
//
// To build it: make sample-plugin. To install: vidyom plugin install <this directory>,
// then grant it in <config-dir>/tenants/<tenant>/plugins.json. Nothing is granted by
// installing it (I261).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/jsonrpc"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
)

const pluginID = "sample-notifier"

type server struct {
	conn *jsonrpc.Conn

	mu       sync.Mutex
	tempDir  string
	finished map[string]bool
	events   map[string]bool
}

func main() {
	s := &server{finished: map[string]bool{}, events: map[string]bool{}}
	s.conn = jsonrpc.NewConn(os.Stdin, os.Stdout, nil, s.handle)
	// Run returns when the daemon closes stdin, which is the signal to exit.
	_ = s.conn.Run(context.Background())
}

func (s *server) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case plugin.MethodInitialize:
		var p plugin.InitializeParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "bad initialize"}
		}
		s.mu.Lock()
		s.tempDir = p.TempDir
		s.mu.Unlock()
		return plugin.InitializeResult{
			ProtocolVersion: plugin.ProtocolVersion,
			PluginID:        pluginID,
			Provides: plugin.Provides{
				HookActions:        []string{"check-source", "record-finish", "notify", "stats"},
				EventSubscriptions: []string{string(event.PublicationCompleted)},
			},
		}, nil
	case plugin.MethodHealth, plugin.MethodShutdown:
		return struct{}{}, nil
	case plugin.MethodHookInvoke:
		var inv plugin.HookInvokeParams
		if err := json.Unmarshal(params, &inv); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "bad invocation"}
		}
		return s.invoke(ctx, inv)
	case plugin.MethodEventDeliver:
		var e plugin.EventDeliverParams
		if err := json.Unmarshal(params, &e); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "bad event"}
		}
		// At-least-once delivery: the event ID is the idempotency key.
		s.mu.Lock()
		s.events[string(e.EventID)] = true
		s.mu.Unlock()
		return struct{}{}, nil
	}
	return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not found"}
}

func (s *server) invoke(ctx context.Context, inv hook.Invocation) (any, error) {
	switch inv.Hook.Action {
	case "check-source":
		// The engine sends a workflow's inputs at workflow.before and a node's resolved
		// parameters at node.before.
		var data struct {
			Title  string `json:"title"`
			Inputs struct {
				Title string `json:"title"`
			} `json:"inputs"`
			Parameters struct {
				Title string `json:"title"`
			} `json:"parameters"`
		}
		_ = json.Unmarshal(inv.Data, &data)
		if data.Title == "" && data.Inputs.Title == "" && data.Parameters.Title == "" {
			return hook.Result{Allow: false, Reason: "the source has no title"}, nil
		}
		return hook.Result{Allow: true}, nil
	case "record-finish":
		return s.recordFinish(inv)
	case "notify":
		return s.notify(ctx, inv)
	case "stats":
		s.mu.Lock()
		defer s.mu.Unlock()
		data, err := json.Marshal(map[string]int{"finished": len(s.finished), "events": len(s.events)})
		if err != nil {
			return nil, err
		}
		return hook.Result{Allow: true, Parameters: data}, nil
	}
	return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "unknown action"}
}

func (s *server) recordFinish(inv hook.Invocation) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished[inv.IdempotencyKey] {
		return hook.Result{Allow: true}, nil // a retry of a hook that already ran
	}
	if s.tempDir == "" {
		return nil, fmt.Errorf("job-temp was not granted")
	}
	line, err := json.Marshal(map[string]string{"job_id": inv.JobID, "point": string(inv.Point)})
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.tempDir, "finished.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	s.finished[inv.IdempotencyKey] = true
	return hook.Result{Allow: true}, nil
}

func (s *server) notify(ctx context.Context, inv hook.Invocation) (any, error) {
	var data struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(inv.Data, &data); err != nil || data.URL == "" {
		return hook.Result{Allow: false, Reason: "notify needs a url"}, nil
	}
	var secret plugin.HostSecretResult
	if err := s.conn.Call(ctx, plugin.MethodHostSecret, plugin.HostSecretParams{Name: "notifier-token"}, &secret); err != nil {
		return nil, fmt.Errorf("get the token: %w", err)
	}
	body, err := json.Marshal(map[string]string{"job_id": inv.JobID})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, data.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret.Value)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: grantedProxy}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("notify: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("notify: status %d", resp.StatusCode)
	}
	_ = s.conn.Notify(ctx, plugin.MethodHostLog, plugin.HostLogParams{Level: "info", Event: "sample.notified", Message: "notified"})
	return hook.Result{Allow: true}, nil
}

// grantedProxy uses the proxy the runtime gives a plugin with a network grant, the only
// way out of its sandbox. Go's own http.ProxyFromEnvironment would skip it for loopback
// destinations, and no direct connection is possible.
func grantedProxy(*http.Request) (*url.URL, error) {
	if p := os.Getenv("HTTPS_PROXY"); p != "" {
		return url.Parse(p)
	}
	return nil, nil
}
