// Package controlnodes implements the control nodes that run inline in the engine
// (pool engine, I7): flow.if, flow.merge, flow.wait, flow.foreach, workflow.execute
// and event.emit. They talk to the running job through Env, which internal/jobs
// provides, so they hold no execution state of their own.
package controlnodes
