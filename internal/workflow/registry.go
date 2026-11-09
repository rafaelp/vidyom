package workflow

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// Entry is one loaded workflow file.
type Entry struct {
	Definition *wfdef.Definition
	Raw        []byte
	SHA256     string
	Source     string
}

// Registry holds workflow files: those embedded in vidyomd plus those of
// <config-dir>/workflows, which add or override by id and version (I22).
type Registry struct {
	validator *Validator
	mu        sync.RWMutex
	byID      map[string]map[string]*Entry
}

// NewRegistry returns an empty registry validating with v, and makes v resolve
// subworkflows through it.
func NewRegistry(v *Validator) *Registry {
	r := &Registry{validator: v, byID: map[string]map[string]*Entry{}}
	v.UseSubworkflows(r)
	return r
}

// LoadRegistry is what a daemon does at start: the shipped files, then the configured
// directories, which add or override by id and version (I22). Anything invalid stops
// it, because a workflow that cannot run should not wait to be requested.
func LoadRegistry(v *Validator, shipped fs.FS, dirs []string) (*Registry, error) {
	r := NewRegistry(v)
	if err := r.LoadFS(shipped, "workflows"); err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		if err := r.LoadDir(dir); err != nil {
			return nil, err
		}
	}
	if err := r.Check(); err != nil {
		return nil, err
	}
	return r, nil
}

// Add stores a file without validating it, because a file may name subworkflows
// that are loaded later. Call Check when everything is loaded.
func (r *Registry) Add(raw []byte, source string) error {
	def, err := wfdef.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", source, err)
	}
	if def.ID == "" || def.Version == "" {
		return fmt.Errorf("%s: a workflow file needs id and version", source)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	versions := r.byID[def.ID]
	if versions == nil {
		versions = map[string]*Entry{}
		r.byID[def.ID] = versions
	}
	versions[def.Version] = &Entry{Definition: def, Raw: append([]byte(nil), raw...), SHA256: wfdef.SHA256(raw), Source: source}
	return nil
}

// LoadFS adds every *.json at the root of fsys.
func (r *Registry) LoadFS(fsys fs.FS, label string) error {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("read %s: %w", label, err)
	}
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return fmt.Errorf("read %s/%s: %w", label, e.Name(), err)
		}
		if err := r.Add(data, label+"/"+e.Name()); err != nil {
			return err
		}
	}
	return nil
}

// LoadDir adds every *.json of a directory. A missing directory is not an error.
func (r *Registry) LoadDir(dir string) error {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return r.LoadFS(os.DirFS(filepath.Clean(dir)), dir)
}

// Check validates everything loaded, and returns every failure.
func (r *Registry) Check() error {
	var problems []string
	for _, e := range r.allEntries() {
		res := r.validator.Validate(e.Raw)
		if err := res.Err(); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", e.Source, err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid workflow files:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

func (r *Registry) allEntries() []*Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Entry
	for _, versions := range r.byID {
		for _, e := range versions {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// Get returns a workflow. An empty version means the latest.
func (r *Registry) Get(id, version string) (*Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	versions := r.byID[id]
	if len(versions) == 0 {
		return nil, false
	}
	if version != "" {
		e, ok := versions[version]
		return e, ok
	}
	var best *Entry
	for _, e := range versions {
		if best == nil || compareVersions(e.Definition.Version, best.Definition.Version) > 0 {
			best = e
		}
	}
	return best, true
}

// LookupWorkflow implements SubworkflowLookup.
func (r *Registry) LookupWorkflow(id, version string) (*wfdef.Definition, bool) {
	e, ok := r.Get(id, version)
	if !ok {
		return nil, false
	}
	return e.Definition, true
}

// List returns the latest version of every workflow, sorted by ID.
func (r *Registry) List() []*Entry {
	r.mu.RLock()
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}
	r.mu.RUnlock()
	sort.Strings(ids)
	out := make([]*Entry, 0, len(ids))
	for _, id := range ids {
		if e, ok := r.Get(id, ""); ok {
			out = append(out, e)
		}
	}
	return out
}

// Triggered returns the latest version of each workflow with this trigger type.
func (r *Registry) Triggered(t wfdef.TriggerType) []*Entry {
	var out []*Entry
	for _, e := range r.List() {
		if e.Definition.Trigger != nil && e.Definition.Trigger.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, _ := strconv.Atoi(pa[i])
		nb, _ := strconv.Atoi(pb[i])
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	return len(pa) - len(pb)
}

// RawLookup finds a workflow's exact bytes for a snapshot bundle.
type RawLookup func(id, version string) ([]byte, bool)

// RawFromRegistry adapts a registry.
func RawFromRegistry(r *Registry) RawLookup {
	return func(id, version string) ([]byte, bool) {
		e, ok := r.Get(id, version)
		if !ok {
			return nil, false
		}
		return e.Raw, true
	}
}

// RawFromBundle adapts a parent snapshot's bundle.
func RawFromBundle(bundle map[string]json.RawMessage) RawLookup {
	return func(id, version string) ([]byte, bool) {
		key := id
		if version != "" {
			key = id + "@" + version
		}
		raw, ok := bundle[key]
		return raw, ok
	}
}

// BuildSnapshot returns the bytes saved with a job: the workflow itself plus, when
// it has subworkflows, their definitions closed over the whole tree (I144). The
// result is what the job's hash covers.
func BuildSnapshot(raw []byte, lookup RawLookup) ([]byte, error) {
	def, err := wfdef.Parse(raw)
	if err != nil {
		return nil, err
	}
	bundle := map[string]json.RawMessage{}
	var collect func(d *wfdef.Definition, path []string) error
	collect = func(d *wfdef.Definition, path []string) error {
		for _, t := range ChildWorkflows(d) {
			for _, seen := range path {
				if seen == t.Workflow {
					return errs.Newf(errs.CodeValidationFailed, "workflows call each other in a cycle: %s", strings.Join(append(path, t.Workflow), " -> "))
				}
			}
			key := t.BundleKey()
			childRaw, ok := bundle[key]
			if !ok {
				data, found := lookup(t.Workflow, t.Version)
				if !found {
					return errs.Newf(errs.CodeValidationFailed, "workflow %s is not available", key)
				}
				childRaw = stripBundle(data)
				bundle[key] = childRaw
			}
			child, err := wfdef.Parse(childRaw)
			if err != nil {
				return err
			}
			if err := collect(child, append(path, t.Workflow)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := collect(def, []string{def.ID}); err != nil {
		return nil, err
	}
	if len(bundle) == 0 {
		return stripBundle(raw), nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	encoded, err := EncodeJSON(bundle)
	if err != nil {
		return nil, err
	}
	doc["subworkflows"] = encoded
	return EncodeJSON(doc)
}

// stripBundle removes a nested bundle, so a child taken from a parent's snapshot
// does not repeat it.
func stripBundle(raw []byte) []byte {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return raw
	}
	if _, ok := doc["subworkflows"]; !ok {
		return raw
	}
	delete(doc, "subworkflows")
	out, err := EncodeJSON(doc)
	if err != nil {
		return raw
	}
	return out
}
