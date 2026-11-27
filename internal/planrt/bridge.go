package planrt

import (
	"context"
	"reflect"
)

// PGTypeInfo holds the unpacked catalog metadata of a tabula.PGType.
type PGTypeInfo struct {
	ID       CatalogType
	Nullable bool
}

// DefaultType holds the unpacked catalog type and original tabula.PGType value
// of a tabula.TypeDefault.
type DefaultType struct {
	Kind CatalogType
	Typ  any
}

// Meta holds statement observation flags decoded from tabula.StatementOption.
type Meta struct {
	Key    string
	Quiet  bool
	Secret bool
}

// ColumnRole mirrors tabula's descriptor column role.
type ColumnRole uint8

const (
	RolePlain ColumnRole = iota
	RoleKey
	RoleScope
	RoleDeleted
	RoleVersion
)

// DeleteKind mirrors tabula's soft-delete configuration kind.
type DeleteKind uint8

const (
	DeleteNone DeleteKind = iota
	DeleteTimestamp
	DeleteBool
	DeleteStatus
)

// DeleteInfo carries the table's soft-delete policy to tabula/plan.
type DeleteInfo struct {
	Kind       DeleteKind
	DeletedVal reflect.Value
	AliveVal   reflect.Value
}

// Column describes one compiled descriptor column for tabula/plan.
type Column[T any] struct {
	Name            string
	Role            ColumnRole
	Once            bool
	Secret          bool
	Updatable       bool
	GoType          reflect.Type
	Nullable        bool
	NeedsColumnType bool
	Target          func(*T) any
	Field           func(*T) any
}

// ShapeRegistration is one compiled shape submitted to Owner.RegisterShapes.
type ShapeRegistration struct {
	Compiled *CompiledShape
	// Shape is a tabula.StatementShape value.
	Shape   any
	Columns map[PhysicalColumn]CatalogType
	// Check validates physical relation sources against the registry's issuer
	// before any registry mutation occurs.
	Check func(issuer *Issuer) error
}

// Owner bridges one registered tabula.Table or tabula.View descriptor to
// package tabula/plan without exposing unforgeable authority primitives.
type Owner[T, S any] struct {
	Issuer         *Issuer
	Entry          *EntryID
	Name           string
	Columns        []Column[T]
	ByName         map[string]int
	HasScopeID     bool
	DefaultTypes   map[reflect.Type]DefaultType
	RegisterShapes func(plan string, shapes []ShapeRegistration) error
	StatementMeta  func(key string, opts any) Meta
	CheckSession   func(s any) error
	EligibleOwner  func(s any) bool
	EncodeScope    func(s any, scopeType CatalogType) (any, error)
	AdaptArray     func(slice any) any
	Authorize      func(s any, shape *CompiledShape, method uint8, args []any, secret []bool, quiet, allSecret, scoped bool) (any, error)
	ValidateAuth   func(s any, shape *CompiledShape, method uint8, scoped bool) error
	Run            func(ctx context.Context, s any, action string, meta Meta, call any, execute func(context.Context) (int64, error)) (int64, error)
	FillAddrs      func(dst []any, row *T) []any
	Addrs          func(row *T) []any
}

// Table extends Owner with physical table metadata needed by tabula/plan.
type Table[T, S any] struct {
	*Owner[T, S]
	RelationScopeKind uint8
	WriteMode         uint8
	ReadOnly          bool
	AllowDelete       bool
	ScopeIdx          int
	DeletedIdx        int
	VersionIdx        int
	Delete            DeleteInfo
	AllowsUpdate      func(string) bool
}
