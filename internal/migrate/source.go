package migrate

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"vector-service/migrations"
)

// migration is one parsed, validated migration: its identity, its original
// embedded bytes, and its execution stream (the original bytes minus exactly
// the outer BEGIN;/COMMIT; framing lines).
type migration struct {
	version  int
	name     string // basename of the migration file
	raw      []byte // original embedded bytes (checksum source)
	checksum string // SHA-256 of raw
	body     []byte // execution stream: raw minus the outer framing
}

// migrationSource supplies the ordered set of migrations to apply. The
// production entry points always use the embedded source; the package-internal
// seam lets package-internal tests substitute a synthetic migration set
// (including deliberately malformed or failing migrations) without an
// exported override.
type migrationSource interface {
	migrations() ([]migration, error)
}

// fileNamePattern matches NNNN_name.sql: a numeric prefix (four or more
// digits, leading zeros permitted), an underscore, a non-empty descriptive
// suffix, and a .sql extension.
var fileNamePattern = regexp.MustCompile(`^([0-9]{4,})_(.+)\.sql$`)

// embeddedSource is the production migration source: the files embedded in
// the binary from the migrations/ directory.
type embeddedSource struct{}

func (embeddedSource) migrations() ([]migration, error) {
	files, err := migrations.Files()
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no embedded migration files found")
	}

	migs := make([]migration, 0, len(files))
	seen := make(map[int]string, len(files))
	for _, f := range files {
		version, _, ok := parseFileName(f.Name)
		if !ok {
			return nil, fmt.Errorf("migration %q does not match the NNNN_name.sql naming convention", f.Name)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q", version, prev, f.Name)
		}
		seen[version] = f.Name

		body, err := stripFraming(f.Name, f.Raw)
		if err != nil {
			return nil, err
		}

		migs = append(migs, migration{
			version:  version,
			name:     f.Name,
			raw:      f.Raw,
			checksum: checksum(f.Raw),
			body:     body,
		})
	}

	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	return migs, nil
}

// parseFileName splits NNNN_name.sql into its numeric version and base name.
func parseFileName(name string) (int, string, bool) {
	m := fileNamePattern.FindStringSubmatch(name)
	if m == nil {
		return 0, "", false
	}
	version, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, "", false
	}
	return version, m[2], true
}

// syntheticSource serves a fixed set of migration files from test code. It is
// package-private: no production entry point accepts an override.
type syntheticSource struct {
	files map[string][]byte
}

func newSyntheticSource(files map[string][]byte) migrationSource {
	return syntheticSource{files: files}
}

func (s syntheticSource) migrations() ([]migration, error) {
	type entry struct {
		name string
		raw  []byte
	}
	entries := make([]entry, 0, len(s.files))
	for name, raw := range s.files {
		entries = append(entries, entry{name: name, raw: raw})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	migs := make([]migration, 0, len(entries))
	seen := make(map[int]string, len(entries))
	for _, e := range entries {
		version, _, ok := parseFileName(e.name)
		if !ok {
			return nil, fmt.Errorf("migration %q does not match the NNNN_name.sql naming convention", e.name)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q", version, prev, e.name)
		}
		seen[version] = e.name

		body, err := stripFraming(e.name, e.raw)
		if err != nil {
			return nil, err
		}

		migs = append(migs, migration{
			version:  version,
			name:     e.name,
			raw:      e.raw,
			checksum: checksum(e.raw),
			body:     body,
		})
	}
	return migs, nil
}
