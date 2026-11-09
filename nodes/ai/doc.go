// Package ainodes implements the catalog's ai.* nodes.
//
// One package holds nodes of two pools (I7). ai.transcribe, ai.embed and
// ai.export_transcript run in vidyom-worker and reach Python only through node.AIEnv.
// The others run in the daemon and reach the database and the LLM only through
// node.DaemonEnv. Nothing here imports a database package, so linking it into
// vidyom-worker keeps that binary cgo-free.
package ainodes
