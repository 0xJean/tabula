package sqlcheck

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/0xJean/tabula"
)

// Query ownership lives in the file preamble, which older snapshot readers
// already ignore. Ordinary statement keys and SQL keep their original format.
const queryHeader = "-- tabula:statement-queries "

type snapshotQueries struct {
	Definition string
	Statements map[string]string
	Shapes     map[string]tabula.StatementShape `json:",omitempty"`
}

func writeQueryHeader(b *strings.Builder, d tabula.Definition) {
	data, _ := json.Marshal(snapshotQueries{Definition: d.Name, Statements: d.StatementQueries, Shapes: d.StatementShapes})
	b.WriteString(queryHeader)
	b.Write(data)
	b.WriteByte('\n')
}

// ParseSnapshot parses a SQL snapshot previously rendered by RenderSnapshot
// into named statements and their recorded query ownership. Standalone tools
// such as tabula check can verify the snapshot against PostgreSQL without
// importing the application's Go packages.
func ParseSnapshot(content string) ([]tabula.Definition, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	queries := make(map[string]snapshotQueries)
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
				owners, recorded := queries[comment]
				if recorded && owners.Statements == nil {
					return nil, fmt.Errorf("sqlcheck.ParseSnapshot: ambiguous query ownership for duplicate definition %q", comment)
				}
				defs = append(defs, tabula.Definition{
					Name:             comment,
					Statements:       map[string]string{},
					StatementQueries: owners.Statements,
					StatementShapes:  owners.Shapes,
				})
				if recorded {
					queries[comment] = snapshotQueries{}
				}
				curDef = &defs[len(defs)-1]
				continue
			}
			// Before the first table header, comment lines are file-level headers.
			if curDef == nil {
				if data, ok := strings.CutPrefix(trimmed, queryHeader); ok {
					var h snapshotQueries
					if err := json.Unmarshal([]byte(data), &h); err != nil {
						return nil, fmt.Errorf("sqlcheck.ParseSnapshot: query header at line %d: %w", i+1, err)
					}
					if h.Definition == "" || len(h.Statements) == 0 || queries[h.Definition].Statements != nil {
						return nil, fmt.Errorf("sqlcheck.ParseSnapshot: invalid or duplicate query header at line %d", i+1)
					}
					queries[h.Definition] = h
				}
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
	for _, def := range defs {
		for key := range def.StatementShapes {
			if def.StatementQueries[key] == "" || def.Statements[key] == "" {
				return nil, fmt.Errorf("sqlcheck.ParseSnapshot: typed shape %q lacks SQL or ownership in %q", key, def.Name)
			}
		}
	}
	for _, h := range queries {
		if len(h.Shapes) != 0 {
			return nil, fmt.Errorf("sqlcheck.ParseSnapshot: missing definition for typed shape header %q", h.Definition)
		}
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
