// Package loki is the Loki log sink (§7.4): JSON over Loki's HTTP push API, gzipped,
// with low-cardinality stream labels (I276).
//
// Tested only against a local mock of the push API (I12). No real Loki is contacted
// by the tests.
package loki
