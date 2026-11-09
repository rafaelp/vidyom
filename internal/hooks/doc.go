// Package hooks applies lifecycle hooks (§8.1, I149): order across the global,
// workflow and node levels, timeouts and failure policies. Running one action is
// the job of hook.ActionRunner, usually a plugin process (D3). A hook is
// synchronous; events are the asynchronous counterpart (package events).
package hooks
