// Package workflows embeds the workflow files that ship with vidyomd (I22).
// <config-dir>/workflows adds or overrides them by id and version.
package workflows

import "embed"

// FS holds every *.json of this directory.
//
//go:embed *.json
var FS embed.FS
