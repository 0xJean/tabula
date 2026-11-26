// Package pgtype declares PostgreSQL catalog types for typed Tabula fields,
// expressions and parameters. Each constructor returns a tabula.PGType.
package pgtype

import (
	"net/netip"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/pgvalue"
)

func catalog[V any](name string) tabula.PGType[V] {
	return tabula.CatalogType[V]("pg_catalog", name, nil)
}

// Bool declares a PostgreSQL boolean.
func Bool() tabula.PGType[bool] { return tabula.Bool() }

// Int2 declares a PostgreSQL smallint.
func Int2() tabula.PGType[int16] { return catalog[int16]("int2") }

// Int4 declares a PostgreSQL integer.
func Int4() tabula.PGType[int32] { return tabula.Int4() }

// Int8 declares a PostgreSQL bigint.
func Int8() tabula.PGType[int64] { return tabula.Int8() }

// Float4 declares a PostgreSQL real.
func Float4() tabula.PGType[float32] { return catalog[float32]("float4") }

// Float8 declares a PostgreSQL double precision value.
func Float8() tabula.PGType[float64] { return catalog[float64]("float8") }

// OID declares a PostgreSQL object identifier.
func OID() tabula.PGType[uint32] { return catalog[uint32]("oid") }

// Text declares PostgreSQL text.
func Text() tabula.PGType[string] { return tabula.Text() }

// BPChar declares PostgreSQL's blank-padded character catalog type.
func BPChar() tabula.PGType[string] { return catalog[string]("bpchar") }

// Bytea declares a PostgreSQL byte string. A nil slice binds SQL NULL and
// a non-nil empty slice binds an empty byte string.
func Bytea() tabula.PGType[[]byte] { return catalog[[]byte]("bytea") }

// Varchar declares PostgreSQL character varying. Supply one length to verify
// the column's character limit. A parameter always uses base varchar.
func Varchar(length ...int) tabula.PGType[string] {
	switch len(length) {
	case 0:
		return catalog[string]("varchar")
	case 1:
		return tabula.Varchar(length[0])
	default:
		panic("pgtype.Varchar: expected zero or one length")
	}
}

// UUID declares a PostgreSQL uuid. Use UUID[string] for UUID text or supply
// a Go representation supported by the driver or a custom input encoder.
func UUID[V any]() tabula.PGType[V] { return catalog[V]("uuid") }

// Numeric declares a PostgreSQL numeric. Numeric[string] binds decimal text
// with its original precision and scale. Driver values and custom encoders
// can supply other exact representations.
func Numeric[V any]() tabula.PGType[V] { return catalog[V]("numeric") }

// Date declares a PostgreSQL date using the year, month and day in the Go
// value's location.
func Date() tabula.PGType[time.Time] { return catalog[time.Time]("date") }

// Timestamp declares a PostgreSQL timestamp without time zone using the
// civil date and clock fields in the Go value's location.
func Timestamp() tabula.PGType[time.Time] { return catalog[time.Time]("timestamp") }

// Timestamptz declares a PostgreSQL timestamp with time zone for an instant.
func Timestamptz() tabula.PGType[time.Time] { return tabula.Timestamptz() }

// Time declares PostgreSQL time without time zone using time-of-day text.
func Time() tabula.PGType[string] { return catalog[string]("time") }

// Timetz declares PostgreSQL time with time zone using time-of-day text with
// an explicit UTC offset.
func Timetz() tabula.PGType[string] { return catalog[string]("timetz") }

// Interval declares a PostgreSQL interval using text. The value can contain
// calendar months, days and fractional seconds.
func Interval() tabula.PGType[string] { return catalog[string]("interval") }

// Inet declares an inet host address. An invalid address binds SQL NULL.
func Inet() tabula.PGType[netip.Addr] {
	return catalog[netip.Addr]("inet").WithEncoder(pgvalue.Network[netip.Addr])
}

// InetPrefix declares an inet address with its network prefix length.
// An invalid prefix binds SQL NULL.
func InetPrefix() tabula.PGType[netip.Prefix] {
	return catalog[netip.Prefix]("inet").WithEncoder(pgvalue.Network[netip.Prefix])
}

// CIDR declares a PostgreSQL cidr network. PostgreSQL requires zero host
// bits. An invalid prefix binds SQL NULL.
func CIDR() tabula.PGType[netip.Prefix] {
	return catalog[netip.Prefix]("cidr").WithEncoder(pgvalue.Network[netip.Prefix])
}

// ArrayOf declares a one-dimensional PostgreSQL array of the given element
// type. Its driver array adapter handles the elements.
func ArrayOf[V any](element tabula.PGType[V]) tabula.PGType[[]V] { return tabula.ArrayOf(element) }
