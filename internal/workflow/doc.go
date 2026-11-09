// Package workflow is the static side of the workflow engine: ${...} references,
// CEL conditions, the node graph, validation with --dry-run, input binding, the
// permissions a workflow needs and the registry of workflow files (§6, D2, D33,
// I22, I23, I141). It has no execution state; internal/jobs runs what this package
// accepts. It imports only contracts, so any component's test may use it.
package workflow
