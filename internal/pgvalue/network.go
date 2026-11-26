// Package pgvalue encodes PostgreSQL values shared by field and input codecs.
package pgvalue

import (
	"fmt"
	"net/netip"
)

// Network encodes a host address or prefix as PostgreSQL network text.
// The Go zero value represents SQL NULL.
func Network[T netip.Addr | netip.Prefix](value T) (any, error) {
	switch n := any(value).(type) {
	case netip.Addr:
		if n.Zone() != "" {
			return nil, fmt.Errorf("tabula: PostgreSQL inet cannot store an IPv6 zone")
		}
		if n.IsValid() {
			return n.String(), nil
		}
	case netip.Prefix:
		if n.IsValid() {
			return n.String(), nil
		}
	}
	return nil, nil
}
