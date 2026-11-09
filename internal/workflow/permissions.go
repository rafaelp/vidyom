package workflow

import (
	"encoding/json"
	"sort"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// nodePermissions are what a node type requires regardless of its parameters (I157).
var nodePermissions = map[string]domain.Permission{
	"catalog.delete":   domain.PermAssetDelete,
	"publish.withdraw": domain.PermPublicationWithdraw,
}

// gatedPublishNodes need publication.create unless a publication gate dominates
// them, because a publisher's decision is what authorizes them (D7, I157).
var gatedPublishNodes = map[string]bool{
	"publish.prepare":  true,
	"publish.upload":   true,
	"publish.finalize": true,
}

// InteractiveOnly lists permissions an event or schedule job never receives: only
// a person starts a deletion or an archive restore (I157).
var InteractiveOnly = map[domain.Permission]bool{
	domain.PermAssetDelete:           true,
	domain.PermStorageArchiveRestore: true,
}

// NodePermissions returns what running one node with these literal parameters
// requires, ignoring gates.
func NodePermissions(n *wfdef.NodeSpec) []domain.Permission {
	var out []domain.Permission
	if p, ok := nodePermissions[n.Type]; ok {
		out = append(out, p)
	}
	if gatedPublishNodes[n.Type] {
		out = append(out, domain.PermPublicationCreate)
	}
	var params struct {
		AllowArchive bool `json:"allowArchive"`
	}
	if json.Unmarshal(n.Parameters, &params) == nil && params.AllowArchive {
		out = append(out, domain.PermStorageArchiveRestore)
	}
	return out
}

// RequiredPermissions walks a definition and its subworkflows and returns what the
// submitter must hold, sorted (I157). A publication gate in a parent covers the
// publishing nodes of the children it dominates.
func RequiredPermissions(def *wfdef.Definition, subs SubworkflowLookup) []domain.Permission {
	set := map[domain.Permission]bool{}
	collectPermissions(def, subs, false, set, map[string]bool{})
	out := make([]domain.Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func collectPermissions(def *wfdef.Definition, subs SubworkflowLookup, gated bool, set map[domain.Permission]bool, active map[string]bool) {
	if active[def.ID] {
		return
	}
	active[def.ID] = true
	defer delete(active, def.ID)

	g, problems := BuildGraph(def)
	if len(problems) > 0 {
		return
	}
	pubGates := gates(def, domain.ApprovalPublication)
	dominated := func(id string) bool {
		if gated {
			return true
		}
		for _, gate := range pubGates {
			if g.Dominates(gate.ID, id) {
				return true
			}
		}
		return false
	}
	for i := range def.Nodes {
		n := &def.Nodes[i]
		for _, p := range NodePermissions(n) {
			if p == domain.PermPublicationCreate && dominated(n.ID) {
				continue
			}
			set[p] = true
		}
		if n.Type != wfdef.TypeWorkflowExecute && n.Type != wfdef.TypeFlowForeach {
			continue
		}
		name, version := executeTarget(n)
		if name == "" || subs == nil {
			continue
		}
		if child, ok := subs.LookupWorkflow(name, version); ok {
			collectPermissions(child, subs, dominated(n.ID), set, active)
		}
	}
}

// BundleLookup resolves subworkflows from a snapshot's bundle (I144).
type BundleLookup struct {
	Bundle map[string]json.RawMessage
}

// LookupWorkflow implements SubworkflowLookup.
func (b BundleLookup) LookupWorkflow(id, version string) (*wfdef.Definition, bool) {
	key := id
	if version != "" {
		key = id + "@" + version
	}
	raw, ok := b.Bundle[key]
	if !ok {
		return nil, false
	}
	def, err := wfdef.Parse(raw)
	if err != nil {
		return nil, false
	}
	return def, true
}
