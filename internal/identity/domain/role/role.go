// Package role defines the Role aggregate — a tenant-scoped named
// bundle of [permission.Permission]s. Future tasks add the aggregate
// struct + factories; this task establishes the package + ID type +
// hierarchy constants only.
package role

const (
	HierarchyLevelDefault = 50
	HierarchyLevelMin     = 0
	HierarchyLevelMax     = 99
	HierarchyLevelNoRole  = 99
)

// ID is the Role primary key (UUIDv7 string form).
type ID string

// IsZero reports whether the ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }
