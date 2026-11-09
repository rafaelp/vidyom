// Package plugins is the plugin runtime (§7.6, §9.4, Phase 4): JSON-RPC 2.0 over stdin
// and stdout to one isolated process per tenant and plugin (D3, D6, I262).
//
// What a plugin can do in this version is provide hook actions and event subscriptions.
// A manifest providing nodes or connector drivers, or a remote plugin over HTTPS, is
// refused with not_implemented (I42, I269). Native Go .so plugins are excluded by the
// spec, and WASM is Future.
//
// Nothing is granted automatically. The manifest declares the most a plugin may hold,
// and each tenant grants explicitly (I261). Filesystem, network and command execution
// are enforced by the OS sandbox (Landlock and seccomp on Linux, Seatbelt on macOS), and
// a host that cannot provide one refuses the plugin unless the operator explicitly
// accepts running without it (I260). Secrets are handed out one at a time by a
// permission-checked reverse call (I266).
//
// Owner: component extensibility (docs/plans/00-master-plan.md, section 5).
package plugins
