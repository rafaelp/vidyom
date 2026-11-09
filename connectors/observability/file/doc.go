// Package file is the local JSONL log sink (§7.4, D38): one JSON object per line,
// daily rotation on the UTC day, retention by age, and a query path so that GET /logs
// works with no external system (§10.1). Details are in decision I275.
package file
