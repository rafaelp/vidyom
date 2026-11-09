// Package approvalnodes implements approval.request, the human gate (R25, D7, I41,
// I158). It runs inline in the engine. The job waits with no process held and no CLI
// connected; service.Approvals in internal/jobs records the decision and resumes it.
package approvalnodes
