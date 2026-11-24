package tabula

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// SQLFiles holds preloaded `.sql` files from an fs.FS (typically embed.FS) so
// long joins can live in dedicated files and fail at start-up when missing.
type SQLFiles struct {
	dir   string
	files map[string]string
}

// LoadSQLFiles reads every `.sql` file under dir in fsys (recursively, keyed by
// slash-separated paths relative to dir). Empty or missing directories return
// an error.
func LoadSQLFiles(fsys fs.FS, dir string) (*SQLFiles, error) {
	if fsys == nil {
		return nil, errors.New("tabula.LoadSQLFiles: filesystem is required")
	}
	cleanDir := path.Clean(strings.TrimSpace(dir))
	if cleanDir == "" || cleanDir == "/" {
		cleanDir = "."
	}
	info, err := fs.Stat(fsys, cleanDir)
	if err != nil {
		return nil, fmt.Errorf("tabula.LoadSQLFiles %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("tabula.LoadSQLFiles %q: not a directory", dir)
	}
	loaded := make(map[string]string)
	err = fs.WalkDir(fsys, cleanDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".sql") {
			return nil
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		rel := p
		if cleanDir != "." {
			rel = strings.TrimPrefix(p, cleanDir+"/")
		}
		sqlText := strings.TrimSpace(string(raw))
		if sqlText == "" {
			return fmt.Errorf("tabula.LoadSQLFiles: %s is empty", rel)
		}
		loaded[rel] = sqlText
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(loaded) == 0 {
		return nil, fmt.Errorf("tabula.LoadSQLFiles %q: no .sql files found", dir)
	}
	return &SQLFiles{dir: cleanDir, files: loaded}, nil
}

// MustSQLFiles calls LoadSQLFiles and panics on error at package initialization.
func MustSQLFiles(fsys fs.FS, dir string) *SQLFiles {
	sf, err := LoadSQLFiles(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sf
}

// Get returns the SQL text for name (relative to dir) and panics if missing.
func (f *SQLFiles) Get(name string) string {
	sqlText, ok := f.Lookup(name)
	if !ok {
		panic(fmt.Sprintf("tabula.SQLFiles: missing SQL file %q", name))
	}
	return sqlText
}

// Lookup returns the SQL text for name (relative to dir) if present.
func (f *SQLFiles) Lookup(name string) (string, bool) {
	if f == nil {
		return "", false
	}
	sqlText, ok := f.files[path.Clean(name)]
	return sqlText, ok
}

// Names returns all loaded relative file names in sorted order.
func (f *SQLFiles) Names() []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, len(f.files))
	for k := range f.files {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
