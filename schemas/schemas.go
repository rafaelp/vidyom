// Package schemas embeds the versioned JSON Schemas and the public.db schema, and
// exposes the node catalog. It imports no database or cgo code, so any binary may
// use it.
package schemas

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rafaelp/vidyom/internal/contract/node"
)

//go:embed *.json public-db/*.sql
var files embed.FS

// Document names.
const (
	Workflow        = "workflow.v1.json"
	PluginManifest  = "plugin-manifest.v1.json"
	ConnectorConfig = "connector-config.v1.json"
	MediaProfile    = "media-profile.v1.json"
	NodeCatalogFile = "node-catalog.v1.json"
	AIWorkerRPC     = "ai-worker-rpc.v1.json"
)

// PublicDBSchemaVersion is the current public.db schema version.
const PublicDBSchemaVersion = 1

// Read returns an embedded file.
func Read(name string) ([]byte, error) { return files.ReadFile(name) }

// PublicDBSchema returns the SQL that creates a public.db (D18).
func PublicDBSchema() (string, error) {
	data, err := files.ReadFile("public-db/v1.sql")
	return string(data), err
}

var (
	compilerOnce sync.Once
	compiler     *jsonschema.Compiler
	compilerErr  error
	compilerMu   sync.Mutex
)

func loadCompiler() (*jsonschema.Compiler, error) {
	compilerOnce.Do(func() {
		c := jsonschema.NewCompiler()
		for _, name := range []string{Workflow, PluginManifest, ConnectorConfig, MediaProfile, NodeCatalogFile, AIWorkerRPC} {
			data, err := files.ReadFile(name)
			if err != nil {
				compilerErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
			if err != nil {
				compilerErr = fmt.Errorf("%s: %w", name, err)
				return
			}
			if err := c.AddResource(name, doc); err != nil {
				compilerErr = fmt.Errorf("%s: %w", name, err)
				return
			}
		}
		compiler = c
	})
	return compiler, compilerErr
}

// Compile compiles a schema: a document name, optionally with a JSON pointer such
// as "node-catalog.v1.json#/nodes/0/parameters".
func Compile(ref string) (*jsonschema.Schema, error) {
	c, err := loadCompiler()
	if err != nil {
		return nil, err
	}
	compilerMu.Lock()
	defer compilerMu.Unlock()
	return c.Compile(ref)
}

// Validate checks a JSON document against a schema reference.
func Validate(ref string, document []byte) error {
	sch, err := Compile(ref)
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(document))
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return sch.Validate(doc)
}

// CatalogEntry is one node type in schemas/node-catalog.v1.json.
type CatalogEntry struct {
	Type          string            `json:"type"`
	TypeVersion   int               `json:"typeVersion"`
	Owner         string            `json:"owner"`
	Pool          node.Pool         `json:"pool"`
	SideEffect    node.SideEffect   `json:"sideEffect"`
	Title         string            `json:"title"`
	Paid          bool              `json:"paid,omitempty"`
	SupportsAbort bool              `json:"supportsAbort,omitempty"`
	Requirements  node.Requirements `json:"requirements,omitzero"`
	Parameters    json.RawMessage   `json:"parameters"`
	Output        json.RawMessage   `json:"output"`
	index         int
}

// ParametersRef returns the schema reference to validate this node's resolved
// parameters with Compile or Validate.
func (e CatalogEntry) ParametersRef() string {
	return fmt.Sprintf("%s#/nodes/%d/parameters", NodeCatalogFile, e.index)
}

// OutputRef returns the schema reference of this node's output.
func (e CatalogEntry) OutputRef() string {
	return fmt.Sprintf("%s#/nodes/%d/output", NodeCatalogFile, e.index)
}

// Descriptor builds a node.Descriptor from the entry, so an implementation can
// return exactly what the catalog declares.
func (e CatalogEntry) Descriptor() node.Descriptor {
	return node.Descriptor{
		Type:          e.Type,
		TypeVersion:   e.TypeVersion,
		Title:         e.Title,
		Owner:         e.Owner,
		Pool:          e.Pool,
		SideEffect:    e.SideEffect,
		SupportsAbort: e.SupportsAbort,
		Paid:          e.Paid,
		Requirements:  e.Requirements,
		Parameters:    e.Parameters,
		Output:        e.Output,
	}
}

// Catalog is the parsed node catalog.
type Catalog struct {
	Nodes []CatalogEntry
}

// Lookup finds a node type and version.
func (c *Catalog) Lookup(nodeType string, version int) (CatalogEntry, bool) {
	for _, e := range c.Nodes {
		if e.Type == nodeType && e.TypeVersion == version {
			return e, true
		}
	}
	return CatalogEntry{}, false
}

// NodeCatalog parses schemas/node-catalog.v1.json.
var NodeCatalog = sync.OnceValues(func() (*Catalog, error) {
	data, err := files.ReadFile(NodeCatalogFile)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Nodes []CatalogEntry `json:"nodes"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", NodeCatalogFile, err)
	}
	for i := range doc.Nodes {
		doc.Nodes[i].index = i
	}
	return &Catalog{Nodes: doc.Nodes}, nil
})

// DescriptorFor returns the catalog's descriptor for a node type at version 1,
// or an error. Implementations use it in Descriptor().
func DescriptorFor(nodeType string, version int) (node.Descriptor, error) {
	cat, err := NodeCatalog()
	if err != nil {
		return node.Descriptor{}, err
	}
	e, ok := cat.Lookup(nodeType, version)
	if !ok {
		return node.Descriptor{}, fmt.Errorf("node type %s@%d is not in the catalog", nodeType, version)
	}
	return e.Descriptor(), nil
}

// CheckDescriptor verifies that an implementation declares what the catalog says.
// Owners call it from their tests for every node they register.
func CheckDescriptor(d node.Descriptor) error {
	want, err := DescriptorFor(d.Type, d.TypeVersion)
	if err != nil {
		return err
	}
	var diffs []string
	if d.Pool != want.Pool {
		diffs = append(diffs, fmt.Sprintf("pool %q, catalog says %q", d.Pool, want.Pool))
	}
	if d.SideEffect != want.SideEffect {
		diffs = append(diffs, fmt.Sprintf("sideEffect %q, catalog says %q", d.SideEffect, want.SideEffect))
	}
	if d.Paid != want.Paid {
		diffs = append(diffs, fmt.Sprintf("paid %v, catalog says %v", d.Paid, want.Paid))
	}
	if len(diffs) > 0 {
		return fmt.Errorf("node %s@%d: %s", d.Type, d.TypeVersion, strings.Join(diffs, "; "))
	}
	return nil
}
