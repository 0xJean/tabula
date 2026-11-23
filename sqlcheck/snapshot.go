package sqlcheck

import (
	"fmt"
	"strings"

	"github.com/0xJean/tabula"
)

// ParseSnapshot parses a SQL snapshot previously rendered by RenderSnapshot
// back into tabula.Definition entries (table name + named statements). This
// allows standalone CI tooling such as `tabula check` to verify a committed
// snapshot directly against PostgreSQL without importing the application's Go
// packages.
func ParseSnapshot(content string) ([]tabula.Definition, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var (
		defs     []tabula.Definition
		curDef   *tabula.Definition
		curKey   string
		sqlLines []string
	)
	flushStmt := func() error {
		if curKey == "" {
			return nil
		}
		if curDef == nil {
			return fmt.Errorf("sqlcheck.ParseSnapshot: statement %q before any table header", curKey)
		}
		raw := strings.TrimSpace(strings.Join(sqlLines, "\n"))
		raw = strings.TrimSuffix(raw, ";")
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return fmt.Errorf("sqlcheck.ParseSnapshot: empty SQL for %s.%s", curDef.Name, curKey)
		}
		if _, dup := curDef.Statements[curKey]; dup {
			return fmt.Errorf("sqlcheck.ParseSnapshot: duplicate statement %s.%s", curDef.Name, curKey)
		}
		curDef.Statements[curKey] = raw
		curKey = ""
		sqlLines = nil
		return nil
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if err := flushStmt(); err != nil {
				return nil, err
			}
			continue
		}
		if isCommentLine(trimmed) {
			comment := strings.TrimSpace(strings.TrimPrefix(trimmed, "--"))
			if err := flushStmt(); err != nil {
				return nil, err
			}
			if isTableHeader(lines, i) {
				defs = append(defs, tabula.Definition{
					Name:       comment,
					Statements: map[string]string{},
				})
				curDef = &defs[len(defs)-1]
				continue
			}
			// Before the first table header, comment lines are file-level headers.
			if curDef == nil {
				continue
			}
			curKey = comment
			continue
		}
		if curDef == nil || curKey == "" {
			return nil, fmt.Errorf("sqlcheck.ParseSnapshot: unexpected SQL at line %d without table/key header", i+1)
		}
		sqlLines = append(sqlLines, line)
	}
	if err := flushStmt(); err != nil {
		return nil, err
	}
	return defs, nil
}

func isCommentLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "-- ") || trimmed == "--"
}

// isTableHeader reports whether lines[i] opens a table section: it is a
// comment line immediately followed by a statement-key comment line and then
// a non-empty SQL line.
func isTableHeader(lines []string, i int) bool {
	if i+2 >= len(lines) {
		return false
	}
	next := strings.TrimSpace(lines[i+1])
	after := strings.TrimSpace(lines[i+2])
	return isCommentLine(next) && after != "" && !isCommentLine(after)
}
