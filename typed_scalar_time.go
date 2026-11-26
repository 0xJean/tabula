package tabula

import (
	"slices"
	"time"
)

// DatePart is a closed EXTRACT or date_trunc unit. Each source type supports
// only its native subset. An invalid unit fails when the plan is defined.
type DatePart uint8

// Native PostgreSQL date/time fields. TimezoneSeconds is the EXTRACT timezone
// field; TimezoneHour and TimezoneMinute are its signed components.
const (
	Microseconds DatePart = iota + 1
	Milliseconds
	Second
	Minute
	Hour
	Day
	Week
	Month
	Quarter
	Year
	Decade
	Century
	Millennium
	Epoch
	DOW
	ISODOW
	DOY
	ISOYear
	TimezoneSeconds
	TimezoneHour
	TimezoneMinute
)

func (u DatePart) sqlName() string {
	names := [...]string{"", "microseconds", "milliseconds", "second", "minute", "hour", "day", "week", "month", "quarter", "year",
		"decade", "century", "millennium", "epoch", "dow", "isodow", "doy", "isoyear", "timezone", "timezone_hour", "timezone_minute"}
	if int(u) >= len(names) {
		return ""
	}
	return names[u]
}

// grammarText is only called with package-owned enum spellings.
func grammarText(value string) scalarOperand {
	kind := Text().id
	return scalarOperand{part: scalarNode{syntax: scalarKeyword, name: "'" + value + "'", result: kind, nulls: nullNever}, kind: kind}
}

// Now returns the PostgreSQL transaction start instant, not a client clock.
func Now() Expr[time.Time] {
	return checkedCall(Timestamptz(), "now", nullNever, expressionStable, nil)
}

// CurrentTimestamp returns CURRENT_TIMESTAMP, stable within the transaction.
func CurrentTimestamp() Expr[time.Time] {
	return scalarResult(Timestamptz(), scalarNode{syntax: scalarKeyword, name: "CURRENT_TIMESTAMP", nulls: nullNever, stability: expressionStable})
}

// CurrentDate returns CURRENT_DATE in the session time zone. It is transaction-stable.
func CurrentDate() Expr[time.Time] {
	return scalarResult(CatalogType[time.Time]("pg_catalog", "date", nil), scalarNode{
		syntax: scalarKeyword, name: "CURRENT_DATE", nulls: nullNever, stability: expressionStable})
}

// DateTrunc truncates timestamp, timestamptz or interval, preserving its type.
// The timestamptz overload observes the session time zone.
func DateTrunc[V any](unit DatePart, value Expr[V]) Expr[V] {
	return dateTrunc(unit, value, nil)
}

// DateTruncAt truncates timestamptz in an explicit, bound time zone.
func DateTruncAt(unit DatePart, value Expr[time.Time], zone Expr[string]) Expr[time.Time] {
	a := textOperand(zone.scalarOperand())
	return dateTrunc(unit, value, &a)
}

func dateTrunc[V any](unit DatePart, value Expr[V], zone *scalarOperand) Expr[V] {
	name := value.kind.id.name
	args := []scalarOperand{grammarText(unit.sqlName()), value.scalarOperand()}
	types := []string{"text", name}
	stability := expressionImmutable
	valid := nativeType(value.kind.id, "timestamp", "timestamptz", "interval") && unit >= Microseconds && unit <= Millennium
	if name == "interval" && unit == Week {
		valid = false
	}
	if name == "timestamptz" {
		stability = expressionStable
	}
	if zone != nil {
		valid = valid && name == "timestamptz"
		args, types = append(args, *zone), append(types, "text")
		stability = expressionImmutable
	}
	v := checkedCall(value.kind, "date_trunc", nullStrict, stability, types, args...)
	if !valid {
		n := v.part.(scalarNode)
		n.err = scalarError("date_trunc", "a supported unit and timestamp, timestamptz or interval (zone requires timestamptz)", args)
		v.part = n
	}
	return v
}

// Timezone converts timestamp to timestamptz and timestamptz to timestamp.
// The native timetz overload preserves timetz and depends on the current date.
func Timezone[V any](zone Expr[string], value Expr[V]) Expr[V] {
	kind := value.kind
	switch {
	case nativeType(value.kind.id, "timestamp"):
		kind.id.name = "timestamptz"
	case nativeType(value.kind.id, "timestamptz"):
		kind.id.name = "timestamp"
	}
	stability := expressionImmutable
	if nativeType(value.kind.id, "timetz") {
		stability = expressionStable
	}
	v := checkedCall(kind, "timezone", nullStrict, stability, []string{"text", value.kind.id.name}, textOperand(zone.scalarOperand()), value.scalarOperand())
	if !nativeType(value.kind.id, "timestamp", "timestamptz", "timetz") {
		n := v.part.(scalarNode)
		n.err = scalarError("timezone", "timestamp, timestamptz or timetz", n.args)
		v.part = n
	}
	return v
}

// Extract returns the selected numeric field. Declare the numeric result's Go
// mapping explicitly, for example pgtype.Numeric[string]().
func Extract[R, V any](result PGType[R], unit DatePart, value Expr[V]) Expr[R] {
	stability := expressionImmutable
	if nativeType(value.kind.id, "timestamptz") {
		stability = expressionStable
	}
	v := checkedCall(result, "extract", nullStrict, stability, []string{"text", value.kind.id.name}, grammarText(unit.sqlName()), value.scalarOperand())
	if !nativeType(result.id, "numeric") || !extractUnit(unit, value.kind.id) {
		n := v.part.(scalarNode)
		n.err = scalarError("extract", "numeric result and a supported source/unit pair", n.args)
		v.part = n
	}
	return v
}

func extractUnit(unit DatePart, kind catalogType) bool {
	if unit.sqlName() == "" || !nativeType(kind, "date", "timestamp", "timestamptz", "time", "timetz", "interval") {
		return false
	}
	if unit >= TimezoneSeconds {
		return nativeType(kind, "timestamptz", "timetz")
	}
	if nativeType(kind, "time", "timetz") {
		return unit <= Hour || unit == Epoch
	}
	if nativeType(kind, "date") {
		return unit >= Day
	}
	if nativeType(kind, "interval") {
		return !slices.Contains([]DatePart{Week, DOW, ISODOW, DOY, ISOYear}, unit)
	}
	return true
}

// ToChar formats a native temporal or numeric value. A date selects the native
// timestamptz overload, preserving session-zone TZ and OF fields. Formatting
// observes session/locale settings and cannot establish a repeatable page key.
func ToChar[V any](value Expr[V], format Expr[string]) Expr[string] {
	arg := value.scalarOperand()
	if nativeType(arg.kind, "date") {
		kind := Timestamptz().id
		arg = scalarOperand{part: scalarNode{syntax: scalarCast, name: "date to timestamptz", result: kind,
			args: []scalarOperand{arg}, stability: expressionStable}, kind: kind, nullable: arg.nullable}
	}
	v := checkedCall(Text(), "to_char", nullStrict, expressionStable, []string{arg.kind.name, "text"}, arg, textOperand(format.scalarOperand()))
	if !nativeType(arg.kind, "timestamp", "timestamptz", "interval", "int4", "int8", "numeric", "float4", "float8") {
		n := v.part.(scalarNode)
		n.err = scalarError("to_char", "a native temporal or numeric formatting overload", n.args)
		v.part = n
	}
	return v
}

// GenRandomUUID generates a non-null UUID with its string Go representation.
// It is volatile: use it in projections and assignments, never stable ordering.
func GenRandomUUID() Expr[string] {
	return GenRandomUUIDAs(CatalogType[string]("pg_catalog", "uuid", nil))
}

// GenRandomUUIDAs generates a UUID using an explicitly declared result mapping.
func GenRandomUUIDAs[V any](result PGType[V]) Expr[V] {
	n := scalarNode{name: "gen_random_uuid", nulls: nullNever, stability: expressionVolatile, functions: []string{"pg_catalog.gen_random_uuid()"}}
	if !nativeType(result.id, "uuid") {
		n.err = scalarError(n.name, "a pg_catalog.uuid result", nil)
	}
	return scalarResult(result, n)
}
