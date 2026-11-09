package domain

// StorageRole is a logical storage role (§7.2). Workflows reference roles,
// never buckets. Clipping must never read from archive.
type StorageRole string

const (
	RoleArchive StorageRole = "archive"
	RoleMaster  StorageRole = "master"
	RoleProxy   StorageRole = "proxy"
)

// StorageRoles lists the logical roles.
var StorageRoles = []StorageRole{RoleArchive, RoleMaster, RoleProxy}

// Valid reports whether r is a known role.
func (r StorageRole) Valid() bool {
	return r == RoleArchive || r == RoleMaster || r == RoleProxy
}
