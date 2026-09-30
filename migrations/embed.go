// Package migrations embeds the released migration files into the binary.
//
// The embedded files are the exact bytes of the migration SQL files in this
// directory. Released migrations are immutable (docs/MIGRATIONS.md): no file
// in this directory is ever modified, and new schema behavior enters only as
// a new NNNN-prefixed file. The compiled service never reads migrations from
// a filesystem path.
package migrations

import (
	"embed"
	"io/fs"
	"path"
	"sort"
)

//go:embed *.sql
var embedded embed.FS

// File is a single embedded migration file. Raw is the unmodified file
// content, exactly as released.
type File struct {
	// Name is the basename of the migration file, for example
	// "0001_shared_vector_schema.sql".
	Name string
	// Raw is the full, unmodified file content.
	Raw []byte
}

// Files returns every embedded migration file, sorted by filename.
func Files() ([]File, error) {
	entries, err := fs.ReadDir(embedded, ".")
	if err != nil {
		return nil, err
	}
	files := make([]File, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if path.Ext(name) != ".sql" {
			continue
		}
		raw, err := fs.ReadFile(embedded, name)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Name: name, Raw: raw})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}
