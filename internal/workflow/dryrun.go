package workflow

import (
	"fmt"
	"sort"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// DryRunEnv is what --dry-run checks a workflow against (§6.2). Nothing here causes
// an effect. Zero fields skip their check, except that a nil Principal check is
// explicit through SkipPermissions.
type DryRunEnv struct {
	Principal       domain.Principal
	SkipPermissions bool
	Tenant          *config.Tenant
	Connectors      []connector.Info
	Secrets         []domain.SecretMeta
	// SecretsKnown says Secrets was read, so a name missing from it is unknown.
	SecretsKnown bool
	Workers      []domain.Worker
	WorkersKnown bool
	Plugins      []domain.PluginRecord
	PluginsKnown bool
}

// capabilityRule is what a node needs from a storage role's driver (§15.4).
type capabilityRule struct {
	Role       domain.StorageRole // empty: the node's "storage" parameter
	Capability string
}

var nodeCapabilities = map[string][]capabilityRule{
	"video.cut":     {{Role: domain.RoleMaster, Capability: "range_reads"}, {Role: domain.RoleMaster, Capability: "direct_access"}},
	"video.preview": {{Role: domain.RoleProxy, Capability: "direct_access"}},
	"storage.put":   {{Capability: "checksum_verification"}},
}

// DryRun returns the issues of the dynamic checks: permissions, connectors and
// capabilities, plugins, resources and the R37 warning. Run it on a Result whose
// static validation passed.
func (v *Validator) DryRun(res Result, env DryRunEnv) []api.ValidationIssue {
	var is issues
	if res.Definition == nil {
		return nil
	}
	defs := v.closure(res.Definition)
	if !env.SkipPermissions {
		checkPermissions(&is, res.Definition, v.subs, env.Principal)
	}
	if env.Tenant != nil {
		checkConnectors(&is, defs, env)
	}
	if env.PluginsKnown {
		checkPlugins(&is, defs, env)
	}
	if env.WorkersKnown {
		v.checkResources(&is, defs, env.Workers)
	}
	CheckClientCredentials(&is, res.Definition, defs, env)
	return is.list
}

// closure returns a definition and every subworkflow reachable from it.
func (v *Validator) closure(def *wfdef.Definition) []*wfdef.Definition {
	out := []*wfdef.Definition{def}
	if v.subs == nil {
		return out
	}
	seen := map[string]bool{def.ID: true}
	for i := 0; i < len(out); i++ {
		for _, t := range ChildWorkflows(out[i]) {
			child, ok := v.subs.LookupWorkflow(t.Workflow, t.Version)
			if !ok || seen[child.ID] {
				continue
			}
			seen[child.ID] = true
			out = append(out, child)
		}
	}
	return out
}

func checkPermissions(is *issues, def *wfdef.Definition, subs SubworkflowLookup, p domain.Principal) {
	for _, perm := range RequiredPermissions(def, subs) {
		if !p.Can(perm) {
			is.err(IssuePermission, "", "", "this workflow needs the permission %s, which the caller does not hold", perm)
		}
	}
}

func literalString(params map[string]any, key string) (string, bool) {
	s, ok := params[key].(string)
	if !ok || HasRef(s) || s == "" {
		return "", false
	}
	return s, true
}

func decodeParams(n *wfdef.NodeSpec) map[string]any {
	if len(n.Parameters) == 0 {
		return nil
	}
	v, err := DecodeJSON(n.Parameters)
	if err != nil {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// usedRoles lists the literal storage roles a node names.
func usedRoles(n *wfdef.NodeSpec, params map[string]any) []domain.StorageRole {
	var out []domain.StorageRole
	if s, ok := literalString(params, "storage"); ok && domain.StorageRole(s).Valid() {
		out = append(out, domain.StorageRole(s))
	}
	if s, ok := literalString(params, "source"); ok {
		if u, err := domain.ParseInputURI(s); err == nil && u.Kind == domain.URIStorage {
			out = append(out, u.Role)
		}
	}
	return out
}

func literalDestinations(params map[string]any) []string {
	var out []string
	if s, ok := literalString(params, "destination"); ok {
		out = append(out, s)
	}
	if list, ok := params["destinations"].([]any); ok {
		for _, e := range list {
			if s, ok := e.(string); ok && !HasRef(s) {
				out = append(out, s)
			}
		}
	}
	return out
}

func infoFor(infos []connector.Info, family connector.Family, driver string) (connector.Info, bool) {
	for _, i := range infos {
		if i.Family == family && i.Driver == driver {
			return i, true
		}
	}
	return connector.Info{}, false
}

func checkConnectors(is *issues, defs []*wfdef.Definition, env DryRunEnv) {
	tenant := env.Tenant
	checkRole := func(nodeID string, role domain.StorageRole) (connector.Info, bool) {
		inst, ok := tenant.Storages.Roles[role]
		if !ok {
			is.err(IssueConnector, nodeID, "", "storage role %s is not configured for this tenant", role)
			return connector.Info{}, false
		}
		info, ok := infoFor(env.Connectors, connector.FamilyStorage, inst.Driver)
		if !ok {
			if env.Connectors != nil {
				is.err(IssueConnector, nodeID, "", "storage role %s uses driver %s, which is not registered", role, inst.Driver)
			}
			return connector.Info{}, false
		}
		if !info.Available {
			is.err(IssueConnector, nodeID, "", "storage driver %s is not available: %s", inst.Driver, info.Reason)
			return info, false
		}
		return info, true
	}
	for _, def := range defs {
		for i := range def.Nodes {
			n := &def.Nodes[i]
			params := decodeParams(n)
			for _, role := range usedRoles(n, params) {
				checkRole(n.ID, role)
			}
			for _, rule := range nodeCapabilities[n.Type] {
				role := rule.Role
				if role == "" {
					s, ok := literalString(params, "storage")
					if !ok {
						continue
					}
					role = domain.StorageRole(s)
				}
				info, ok := checkRole(n.ID, role)
				if !ok {
					continue
				}
				if inst := tenant.Storages.Roles[role]; rule.Capability == "direct_access" && inst.Usage == config.UsageBackupOnly {
					is.err(IssueCapability, n.ID, "", "%s needs direct access to role %s, which is configured as backup_only (§7.2)", n.Type, role)
					continue
				}
				if allowed, declared := info.Capabilities[rule.Capability]; declared && !allowed {
					is.err(IssueCapability, n.ID, "", "%s needs %s on role %s, and driver %s does not support it (§15.4)", n.Type, rule.Capability, role, info.Driver)
				}
			}
			for _, dest := range literalDestinations(params) {
				d, ok := tenant.Publishing.Destinations[dest]
				if !ok {
					is.err(IssueConnector, n.ID, "", "destination %s is not configured for this tenant", dest)
					continue
				}
				info, ok := infoFor(env.Connectors, connector.FamilyPublishing, d.Driver)
				if !ok {
					if env.Connectors != nil {
						is.err(IssueConnector, n.ID, "", "destination %s uses driver %s, which is not registered", dest, d.Driver)
					}
					continue
				}
				if !info.Available {
					is.err(IssueConnector, n.ID, "", "destination %s: driver %s is not available: %s", dest, d.Driver, info.Reason)
				}
			}
		}
	}
}

func hookDecls(set *wfdef.HookSet) []wfdef.HookDecl {
	if set == nil {
		return nil
	}
	var out []wfdef.HookDecl
	for _, l := range [][]wfdef.HookDecl{set.Before, set.Start, set.After, set.Finish, set.Error, set.Cancel} {
		out = append(out, l...)
	}
	return out
}

func checkPlugins(is *issues, defs []*wfdef.Definition, env DryRunEnv) {
	known := map[string]domain.PluginRecord{}
	for _, p := range env.Plugins {
		known[p.ID] = p
	}
	check := func(nodeID string, h wfdef.HookDecl) {
		p, ok := known[h.Plugin]
		if !ok {
			is.err(IssuePlugin, nodeID, "", "hook %s names plugin %s, which is not installed", h.ID, h.Plugin)
			return
		}
		if !p.Enabled {
			is.err(IssuePlugin, nodeID, "", "hook %s names plugin %s, which is disabled", h.ID, h.Plugin)
		}
	}
	for _, def := range defs {
		for _, h := range hookDecls(def.Hooks) {
			check("", h)
		}
		for i := range def.Nodes {
			for _, h := range hookDecls(def.Nodes[i].Hooks) {
				check(def.Nodes[i].ID, h)
			}
		}
	}
}

func (v *Validator) checkResources(is *issues, defs []*wfdef.Definition, workers []domain.Worker) {
	for _, def := range defs {
		for i := range def.Nodes {
			n := &def.Nodes[i]
			d, ok := v.Descriptor(n.Type, n.TypeVersion)
			if !ok || d.Pool == node.PoolEngine || d.Pool == node.PoolDaemon {
				continue
			}
			inPool, fits := false, false
			for _, w := range workers {
				if !containsString(w.Pools, string(d.Pool)) {
					continue
				}
				inPool = true
				if Satisfies(w.Resources, d.Requirements) {
					fits = true
					break
				}
			}
			switch {
			case !inPool:
				is.warn(IssueResources, n.ID, "", "no registered worker serves pool %s, which %s needs", d.Pool, n.Type)
			case !fits:
				is.warn(IssueResources, n.ID, "", "no registered worker of pool %s has the resources %s declares (D40)", d.Pool, n.Type)
			}
		}
	}
}

func containsString(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// Satisfies reports whether declared resources meet a node's requirements (D40).
func Satisfies(r domain.WorkerResources, q node.Requirements) bool {
	return r.CPUCores >= q.CPUCores && r.MemoryMB >= q.MemoryMB && r.DiskMB >= q.DiskMB && (!q.GPU || r.GPU)
}

// credentialRefs returns the credentialsRef values a workflow tree reaches, with
// where each comes from: literal parameters, the storage roles it names, the
// destinations it names, and the public library.
func credentialRefs(defs []*wfdef.Definition, tenant *config.Tenant) map[string]string {
	refs := map[string]string{}
	add := func(ref, from string) {
		if ref != "" {
			if _, dup := refs[ref]; !dup {
				refs[ref] = from
			}
		}
	}
	var walk func(v any, from string)
	walk = func(v any, from string) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if s, ok := e.(string); ok && k == "credentialsRef" && !HasRef(s) {
					add(s, from)
				}
				walk(e, from)
			}
		case []any:
			for _, e := range x {
				walk(e, from)
			}
		}
	}
	for _, def := range defs {
		for i := range def.Nodes {
			n := &def.Nodes[i]
			params := decodeParams(n)
			from := "node " + n.ID
			walk(any(params), from)
			if tenant == nil {
				continue
			}
			for _, role := range usedRoles(n, params) {
				if inst, ok := tenant.Storages.Roles[role]; ok {
					add(inst.CredentialsRef, fmt.Sprintf("storage role %s used by node %s", role, n.ID))
				}
			}
			for _, dest := range literalDestinations(params) {
				if d, ok := tenant.Publishing.Destinations[dest]; ok {
					add(d.CredentialsRef, fmt.Sprintf("destination %s used by node %s", dest, n.ID))
				}
			}
			if n.Type == "catalog.public.sync" && tenant.Catalogs.PublicLibrary != nil {
				add(tenant.Catalogs.PublicLibrary.CredentialsRef, "public library used by node "+n.ID)
			}
		}
	}
	return refs
}

// CheckClientCredentials warns when a workflow that runs with no connected client
// references a client credential, or one that does not exist (R37, §15.6).
func CheckClientCredentials(is *issues, def *wfdef.Definition, defs []*wfdef.Definition, env DryRunEnv) {
	if def.Trigger == nil || (def.Trigger.Type != wfdef.TriggerEvent && def.Trigger.Type != wfdef.TriggerSchedule) {
		return
	}
	location := map[string]domain.SecretLocation{}
	for _, s := range env.Secrets {
		location[s.Name] = s.Location
	}
	refs := credentialRefs(defs, env.Tenant)
	names := make([]string, 0, len(refs))
	for k := range refs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		loc, known := location[name]
		switch {
		case known && loc == domain.SecretClient:
			is.warn(IssueCredential, "", "", "%s references the client credential %s through %s; a job with a %s trigger runs with no client connected and can only use server or delegated credentials (R37)", def.ID, name, refs[name], def.Trigger.Type)
		case !known && env.SecretsKnown:
			is.warn(IssueUnknownRef, "", "", "%s references credential %s through %s, which is not in the tenant vault; a job with a %s trigger can only use server or delegated credentials (R37)", def.ID, name, refs[name], def.Trigger.Type)
		}
	}
}
