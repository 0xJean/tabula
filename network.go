package tabula

import (
	"database/sql/driver"
	"fmt"
	"net/netip"
	"reflect"
	"strings"

	"github.com/0xJean/tabula/internal/pgvalue"
)

type network interface{ netip.Addr | netip.Prefix }
type networkValue[T network] struct {
	value    *T
	optional **T
}

func networkType(t reflect.Type) bool {
	return t == reflect.TypeFor[netip.Addr]() || t == reflect.TypeFor[netip.Prefix]()
}

func networkTypes() map[reflect.Type]func(any) any {
	return map[reflect.Type]func(any) any{
		reflect.TypeFor[netip.Addr]():    wrapNetwork[netip.Addr],
		reflect.TypeFor[*netip.Addr]():   wrapNetwork[netip.Addr],
		reflect.TypeFor[netip.Prefix]():  wrapNetwork[netip.Prefix],
		reflect.TypeFor[*netip.Prefix](): wrapNetwork[netip.Prefix],
	}
}

func wrapNetwork[T network](p any) any {
	if dst, ok := p.(*T); ok {
		return networkValue[T]{value: dst}
	}
	return networkValue[T]{optional: p.(**T)}
}

func (v networkValue[T]) Scan(src any) error {
	var result T
	if src != nil {
		var text string
		switch s := src.(type) {
		case string:
			text = s
		case []byte:
			text = string(s)
		default:
			return fmt.Errorf("tabula: cannot scan %T as a network address", src)
		}
		var prefix netip.Prefix
		var err error
		if strings.Contains(text, "/") {
			prefix, err = netip.ParsePrefix(text)
		} else {
			var addr netip.Addr
			addr, err = netip.ParseAddr(text)
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		if err != nil {
			return err
		}
		switch p := any(&result).(type) {
		case *netip.Prefix:
			*p = prefix
		case *netip.Addr:
			if prefix.Bits() != prefix.Addr().BitLen() {
				return fmt.Errorf("tabula: %s needs a netip.Prefix", prefix)
			}
			*p = prefix.Addr()
		}
	}
	if v.value != nil {
		*v.value = result
	} else if src == nil {
		*v.optional = nil
	} else {
		*v.optional = &result
	}
	return nil
}

func (v networkValue[T]) Value() (driver.Value, error) {
	p := v.value
	if v.optional != nil {
		p = *v.optional
	}
	if p == nil {
		return nil, nil
	}
	return pgvalue.Network(*p)
}
