// Package events is the persistent Event Bus (§8.2, I156). An event commits with the
// state change that caused it, in the tenant's outbox. A dispatcher turns each one
// into a delivery per subscriber, tracked in the tenant database, and delivers at
// least once. Workflows subscribe through an event trigger, plugins through their
// manifest. Depth and fan-out limits stop loops. An event never blocks a workflow;
// a hook, in package hooks, is the synchronous counterpart.
package events
