package plan

import (
	"github.com/0xJean/tabula"

	"errors"
	"slices"
)

// RowLockStrength selects the PostgreSQL locking clause on a physical relation.
type RowLockStrength uint8

const (
	ForUpdate RowLockStrength = iota + 1
	ForNoKeyUpdate
	ForShare
	ForKeyShare
)

// LockWait selects how an operation handles contention.
type LockWait uint8

const (
	// Wait blocks until PostgreSQL grants the lock or the operation ends.
	Wait LockWait = iota + 1
	// SkipLocked excludes conflicting rows from a locking read.
	SkipLocked
	// NoWait raises PostgreSQL lock_not_available on a conflicting row.
	NoWait
	// Try returns false immediately on an advisory lock conflict.
	Try
)

// RowLock identifies a locking strength, a specific occurrence and a wait mode.
type RowLock struct {
	strength RowLockStrength
	target   *occurrence
	wait     LockWait
	declared bool
}

// ForRows declares a lock on target. Wait, SkipLocked and NoWait are valid.
func ForRows[T any](strength RowLockStrength, target Rel[T], wait LockWait) RowLock {
	return RowLock{strength: strength, target: target.source, wait: wait, declared: true}
}

// clause applies only to a physical row selection. It records UPDATE on any
// one column independently of the SELECT grants derived from expressions.
func (l RowLock) clause(c *typedCompiler, meta *tabula.StatementShape) (string, error) {
	if _, ok := c.sources[l.target]; !ok {
		return "", errors.New("tabula: row lock target is outside this query")
	}
	if err := l.target.checkCommand("UPDATE"); err != nil {
		return "", err
	}
	alias := l.target.alias
	if c.block != nil {
		if err := c.block.resolvePhysical(l.target, 0); err != nil {
			return "", err
		}
		if c.block.physicalAliases[l.target] != "" {
			alias = c.block.physicalAliases[l.target]
		}
	}
	var strength, wait string
	switch l.strength {
	case ForUpdate:
		strength = "UPDATE"
	case ForNoKeyUpdate:
		strength = "NO KEY UPDATE"
	case ForShare:
		strength = "SHARE"
	case ForKeyShare:
		strength = "KEY SHARE"
	default:
		return "", errors.New("tabula: invalid row lock strength")
	}
	switch l.wait {
	case Wait:
	case SkipLocked:
		wait = " SKIP LOCKED"
	case NoWait:
		wait = " NOWAIT"
	default:
		return "", errors.New("tabula: a row lock requires Wait, SkipLocked or NoWait")
	}
	meta.Effects |= tabula.EffectRowLock
	if !slices.Contains(meta.AnyColumnUpdate, l.target.table.SQL()) {
		meta.AnyColumnUpdate = append(meta.AnyColumnUpdate, l.target.table.SQL())
	}
	return " FOR " + strength + " OF " + typedQuote(alias) + wait, nil
}
