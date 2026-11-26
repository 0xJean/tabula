package tabula

// FirstMode declares how a read plan may select its first result.
type FirstMode uint8

const (
	// FirstDisabled leaves first-row selection disabled.
	FirstDisabled FirstMode = iota
	// OrderedFirst requires a declared order for first-row selection.
	OrderedFirst
	// UnorderedFirst permits the first row of an explicitly unordered read.
	UnorderedFirst
)

// ReadForms registers a bounded read for exact or first-row selection.
// Exact and First share one shape per eligible filter, order and scope.
type ReadForms struct {
	Exact bool
	First FirstMode
}
