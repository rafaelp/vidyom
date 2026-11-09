// Package local is the local storage connector (I9): each role is a directory on
// a local disk or NAS, with the tenant's prefix inside it. It runs the whole
// scenario with no cloud account and offers an access log that proves which roles
// a job read (I188).
//
// Layout under an instance's root:
//
//	<root>/<prefix>/<tenant>/<key>                  the object
//	<root>/.vidyom/meta/<prefix>/<tenant>/<key>.json checksum and attributes
//	<root>/.vidyom/gens/<prefix>/<tenant>/<key>/     earlier generations
//	<root>/.vidyom/tmp/                             writes in progress
//
// Instance parameters:
//
//	accessLog            absolute path of a JSONL file; empty means no log
//	versioning           keep earlier generations on overwrite; default true
//	disableCapabilities  capability names to declare absent, which makes the
//	                     matching operations answer capability_unsupported (§15.4)
package local
