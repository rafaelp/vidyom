package domain

// Role is a per-tenant role (D7). Roles are bundles of granular permissions (D48).
type Role string

const (
	RoleViewer    Role = "viewer"
	RoleEditor    Role = "editor"
	RolePublisher Role = "publisher"
	RoleAdmin     Role = "admin"
)

// Roles lists roles from least to most privileged.
var Roles = []Role{RoleViewer, RoleEditor, RolePublisher, RoleAdmin}

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	for _, known := range Roles {
		if known == r {
			return true
		}
	}
	return false
}

func (r Role) rank() int {
	for i, known := range Roles {
		if known == r {
			return i + 1
		}
	}
	return 0
}

// AtLeast reports whether r is other or more privileged.
func (r Role) AtLeast(other Role) bool { return r.rank() >= other.rank() && r.rank() > 0 }

// Has reports whether the role's bundle includes perm.
func (r Role) Has(perm Permission) bool {
	for _, level := range Roles {
		if level.rank() > r.rank() {
			break
		}
		for _, p := range roleBundles[level] {
			if p == perm {
				return true
			}
		}
	}
	return false
}

// Permission is a granular capability (D48). The audit records which permission
// authorized each action.
type Permission string

const (
	PermAssetRead      Permission = "asset.read"
	PermAssetWrite     Permission = "asset.write"
	PermAssetDelete    Permission = "asset.delete"
	PermAssetAIConsent Permission = "asset.ai_consent"
	PermAssetSensitive Permission = "asset.sensitive"
	PermUploadCreate   Permission = "upload.create"

	PermWorkflowRead Permission = "workflow.read"
	PermWorkflowRun  Permission = "workflow.run"
	PermNodeRead     Permission = "node.read"
	PermNodeRun      Permission = "node.run"

	PermJobRead               Permission = "job.read"
	PermJobControl            Permission = "job.control"
	PermJobResolve            Permission = "job.resolve"
	PermJobResolvePublication Permission = "job.resolve.publication"

	PermApprovalRead                Permission = "approval.read"
	PermApprovalDecideClipSelection Permission = "approval.decide.clip_selection"
	PermApprovalDecidePublication   Permission = "approval.decide.publication"

	PermPublicationRead     Permission = "publication.read"
	PermPublicationCreate   Permission = "publication.create"
	PermPublicationWithdraw Permission = "publication.withdraw"

	PermSearch Permission = "search.read"

	PermStorageRead           Permission = "storage.read"
	PermStorageTest           Permission = "storage.test"
	PermStorageCopy           Permission = "storage.copy"
	PermStorageArchiveRestore Permission = "storage.archive.restore"

	PermSecretRead  Permission = "secret.read"
	PermSecretWrite Permission = "secret.write"

	PermConnectorRead Permission = "connector.read"
	PermPluginRead    Permission = "plugin.read"
	PermPluginInstall Permission = "plugin.install"
	PermHookRead      Permission = "hook.read"
	PermHookTest      Permission = "hook.test"
	PermEventRead     Permission = "event.read"

	PermUserRead     Permission = "user.read"
	PermUserManage   Permission = "user.manage"
	PermDeviceRead   Permission = "device.read"
	PermDeviceManage Permission = "device.manage"

	PermBudgetRead     Permission = "budget.read"
	PermBudgetManage   Permission = "budget.manage"
	PermBudgetOverride Permission = "budget.override"

	PermAuditRead   Permission = "audit.read"
	PermAuditExport Permission = "audit.export"

	PermMetricsRead         Permission = "metrics.read"
	PermObservabilityRead   Permission = "observability.read"
	PermObservabilityManage Permission = "observability.manage"

	// System-level permissions belong to system admins, not to tenant roles.
	PermTenantCreate Permission = "system.tenant.create"
	PermSystemAdmin  Permission = "system.admin"
)

// SystemLevel reports whether the permission is outside any tenant role.
func (p Permission) SystemLevel() bool {
	return p == PermTenantCreate || p == PermSystemAdmin
}

// roleBundles lists what each level adds to the one below it.
var roleBundles = map[Role][]Permission{
	RoleViewer: {
		PermAssetRead, PermWorkflowRead, PermNodeRead, PermJobRead, PermApprovalRead,
		PermPublicationRead, PermSearch, PermStorageRead, PermConnectorRead,
		PermPluginRead, PermHookRead, PermEventRead, PermBudgetRead, PermObservabilityRead,
	},
	RoleEditor: {
		PermAssetWrite, PermAssetSensitive, PermUploadCreate, PermWorkflowRun,
		PermNodeRun, PermJobControl, PermJobResolve, PermApprovalDecideClipSelection,
	},
	RolePublisher: {
		PermApprovalDecidePublication, PermPublicationCreate, PermPublicationWithdraw,
		PermJobResolvePublication,
	},
	RoleAdmin: {
		PermAssetDelete, PermAssetAIConsent, PermStorageTest, PermStorageCopy,
		PermStorageArchiveRestore, PermSecretRead, PermSecretWrite, PermPluginInstall,
		PermHookTest, PermUserRead, PermUserManage, PermDeviceRead, PermDeviceManage,
		PermBudgetManage, PermBudgetOverride, PermAuditRead, PermAuditExport,
		PermMetricsRead, PermObservabilityManage,
	},
}

// PermissionsOf returns the full bundle of a role.
func PermissionsOf(r Role) []Permission {
	var out []Permission
	for _, level := range Roles {
		if level.rank() > r.rank() {
			break
		}
		out = append(out, roleBundles[level]...)
	}
	return out
}
