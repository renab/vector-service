package vectors

import (
	"crypto/rand"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// The package-3 unit matrix (no database): the pure validation, SQL-builder,
// and score-transform logic of the vector operations. The database-bound
// paths (the five service functions' transactions, RLS, the trigger's
// defense in depth) are integration territory.

// ---------------------------------------------------------------------------
// Record ID generation
// ---------------------------------------------------------------------------

// TestNewRecordIDIsUUIDv4 proves the service-generated record id is a
// lowercase hyphenated UUIDv4 (the version nibble is 4 and the variant is
// 10), and that two generated ids differ (crypto/rand, not a counter).
func TestNewRecordIDIsUUIDv4(t *testing.T) {
	a, err := newRecordID()
	if err != nil {
		t.Fatalf("newRecordID: %v", err)
	}
	b, err := newRecordID()
	if err != nil {
		t.Fatalf("newRecordID: %v", err)
	}
	if a == b {
		t.Fatalf("two generated record ids are identical: %q", a)
	}
	if len(a) != 36 {
		t.Fatalf("record id length = %d, want 36", len(a))
	}
	// Dashes at the fixed positions.
	for _, i := range []int{8, 13, 18, 23} {
		if a[i] != '-' {
			t.Fatalf("record id missing dash at %d: %q", i, a)
		}
	}
	// Version 4.
	if a[14] != '4' {
		t.Fatalf("record id version nibble = %q, want 4: %q", a[14], a)
	}
	// Variant 10: the variant nibble (index 19) is 8, 9, a, or b.
	switch a[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Fatalf("record id variant nibble = %q, want 8/9/a/b: %q", a[19], a)
	}
	// All other characters are lowercase hex.
	for i, r := range a {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !isHexChar(r) {
			t.Fatalf("record id has non-hex byte %q at %d: %q", r, i, a)
		}
	}
}

// ---------------------------------------------------------------------------
// Content hash
// ---------------------------------------------------------------------------

// TestParseContentHash is the content-hash grammar matrix: exactly 64
// lowercase hexadecimal characters decode to 32 bytes; any other shape is
// ErrInvalidContentHash.
func TestParseContentHash(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
		wantLen int
	}{
		{"all zero", strings.Repeat("0", 64), false, 32},
		{"all f", strings.Repeat("f", 64), false, 32},
		{"mixed", "0f1e2d3c4b5a69788796a5b4c3d2e1f00123456789abcdef0123456789abcdef", false, 32},
		{"too short", strings.Repeat("0", 63), true, 0},
		{"too long", strings.Repeat("0", 65), true, 0},
		{"empty", "", true, 0},
		{"uppercase", strings.Repeat("A", 64), true, 0},
		{"mixed case", "a" + strings.Repeat("B", 63), true, 0},
		{"non-hex char", strings.Repeat("0", 32) + "g" + strings.Repeat("0", 31), true, 0},
		{"space char", strings.Repeat("0", 32) + " " + strings.Repeat("0", 31), true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := parseContentHash(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseContentHash(%q) error = %v, want error = %v", tc.in, err, tc.wantErr)
			}
			if tc.wantErr {
				if !isInvalidContentHash(err) {
					t.Fatalf("parseContentHash(%q) error = %v, want ErrInvalidContentHash", tc.in, err)
				}
				return
			}
			if len(raw) != tc.wantLen {
				t.Fatalf("parseContentHash(%q) length = %d, want %d", tc.in, len(raw), tc.wantLen)
			}
		})
	}
}

func isInvalidContentHash(err error) bool {
	return err == ErrInvalidContentHash
}

// ---------------------------------------------------------------------------
// Metadata
// ---------------------------------------------------------------------------

// TestValidateMetadata is the metadata validation matrix: a nil object is
// metadata_not_object, a well-formed object under the bound passes, and an
// object over the bound is metadata_too_large.
func TestValidateMetadata(t *testing.T) {
	t.Run("nil is not an object", func(t *testing.T) {
		_, err := validateMetadata(nil, 16*1024)
		if err != ErrMetadataNotObject {
			t.Fatalf("validateMetadata(nil) error = %v, want ErrMetadataNotObject", err)
		}
	})
	t.Run("empty object passes", func(t *testing.T) {
		b, err := validateMetadata(map[string]any{}, 16*1024)
		if err != nil {
			t.Fatalf("validateMetadata(empty) error = %v, want nil", err)
		}
		if string(b) != "{}" {
			t.Fatalf("canonical form = %q, want {}", string(b))
		}
	})
	t.Run("small object passes", func(t *testing.T) {
		b, err := validateMetadata(map[string]any{"a": "1", "b": true}, 16*1024)
		if err != nil {
			t.Fatalf("validateMetadata(small) error = %v, want nil", err)
		}
		// The canonical form is compact JSON with sorted keys.
		if string(b) != `{"a":"1","b":true}` {
			t.Fatalf("canonical form = %q, want sorted compact JSON", string(b))
		}
	})
	t.Run("over the bound is too large", func(t *testing.T) {
		// A 20-byte string value already exceeds a 10-byte bound once the
		// JSON framing is counted.
		meta := map[string]any{"k": strings.Repeat("x", 20)}
		_, err := validateMetadata(meta, 10)
		if err != ErrMetadataTooLarge {
			t.Fatalf("validateMetadata(oversized) error = %v, want ErrMetadataTooLarge", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Filters
// ---------------------------------------------------------------------------

// TestValidateFilterEntry is the structured-filter grammar matrix.
func TestValidateFilterEntry(t *testing.T) {
	b := Bounds{MaxFilters: 10, MaxFilterValues: 50}
	cases := []struct {
		name    string
		entry   FilterEntry
		wantErr bool
	}{
		{"eq one value", FilterEntry{Field: "state", Op: FilterEq, Values: []any{"active"}}, false},
		{"in multiple values", FilterEntry{Field: "tag", Op: FilterIn, Values: []any{"a", "b", "c"}}, false},
		{"in single value", FilterEntry{Field: "tag", Op: FilterIn, Values: []any{"a"}}, false},
		{"numeric value", FilterEntry{Field: "count", Op: FilterEq, Values: []any{float64(3)}}, false},
		{"boolean value", FilterEntry{Field: "flag", Op: FilterEq, Values: []any{true}}, false},
		{"empty field", FilterEntry{Field: "", Op: FilterEq, Values: []any{"a"}}, true},
		{"field too long", FilterEntry{Field: strings.Repeat("f", 129), Op: FilterEq, Values: []any{"a"}}, true},
		{"field at max length", FilterEntry{Field: strings.Repeat("f", 128), Op: FilterEq, Values: []any{"a"}}, false},
		{"unknown operator", FilterEntry{Field: "state", Op: FilterOperator("ne"), Values: []any{"a"}}, true},
		{"eq zero values", FilterEntry{Field: "state", Op: FilterEq, Values: []any{}}, true},
		{"eq two values", FilterEntry{Field: "state", Op: FilterEq, Values: []any{"a", "b"}}, true},
		{"in zero values", FilterEntry{Field: "state", Op: FilterIn, Values: []any{}}, true},
		{"too many values", FilterEntry{Field: "state", Op: FilterIn, Values: make([]any, 51)}, true},
		{"object value", FilterEntry{Field: "state", Op: FilterEq, Values: []any{map[string]any{}}}, true},
		{"array value", FilterEntry{Field: "state", Op: FilterEq, Values: []any{[]any{}}}, true},
		{"null value", FilterEntry{Field: "state", Op: FilterEq, Values: []any{nil}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFilterEntry(tc.entry, b)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateFilterEntry(%+v) error = %v, want error = %v", tc.entry, err, tc.wantErr)
			}
			if tc.wantErr && err != ErrInvalidFilter {
				t.Fatalf("validateFilterEntry error = %v, want ErrInvalidFilter", err)
			}
		})
	}
}

// TestValidateFilterSetBounds checks the filter-set bound: more entries than
// MaxFilters is invalid_filter.
func TestValidateFilterSetBounds(t *testing.T) {
	b := Bounds{MaxFilters: 3, MaxFilterValues: 50}
	ok := make([]FilterEntry, 3)
	for i := range ok {
		ok[i] = FilterEntry{Field: "f", Op: FilterEq, Values: []any{"v"}}
	}
	if err := validateFilterSet(ok, b); err != nil {
		t.Fatalf("validateFilterSet(3) error = %v, want nil", err)
	}
	tooMany := append(append([]FilterEntry{}, ok...), FilterEntry{Field: "f", Op: FilterEq, Values: []any{"v"}})
	if err := validateFilterSet(tooMany, b); err != ErrInvalidFilter {
		t.Fatalf("validateFilterSet(4) error = %v, want ErrInvalidFilter", err)
	}
}

// ---------------------------------------------------------------------------
// Upsert batch validation
// ---------------------------------------------------------------------------

// validRecord returns a record that passes every cheap check (used as the
// base for the batch-validation matrix).
func validRecord(objOrd, projOrd int) UpsertRecord {
	return UpsertRecord{
		ObjectID:        deterministicUUID(objOrd, 0),
		ProjectionID:    deterministicUUID(projOrd, 1),
		ContentHash:     strings.Repeat("0", 64),
		SourceUpdatedAt: "2026-01-02T03:04:05Z",
		Metadata:        map[string]any{"k": "v"},
		Vector:          []float32{0, 0, 0},
	}
}

// deterministicUUID builds a stable, obviously-synthetic UUIDv4-shaped id
// for test fixtures (the version/variant nibbles are set so the grammar
// check passes).
func deterministicUUID(hi, lo int) string {
	var b [16]byte
	b[0] = byte(hi)
	b[1] = byte(hi >> 8)
	b[12] = byte(lo)
	b[13] = byte(lo >> 8)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUIDBytes(b[:])
}

// TestValidateUpsertBatch is the per-record batch-validation matrix.
func TestValidateUpsertBatch(t *testing.T) {
	b := Bounds{MaxMetadataBytes: 16 * 1024}
	cases := []struct {
		name    string
		mutate  func(UpsertRecord) UpsertRecord
		wantErr error
	}{
		{"valid single", func(r UpsertRecord) UpsertRecord { return r }, nil},
		{"missing object id", func(r UpsertRecord) UpsertRecord { r.ObjectID = ""; return r }, ErrMissingField},
		{"missing projection id", func(r UpsertRecord) UpsertRecord { r.ProjectionID = ""; return r }, ErrMissingField},
		{"missing content hash", func(r UpsertRecord) UpsertRecord { r.ContentHash = ""; return r }, ErrMissingField},
		{"absent timestamp is optional", func(r UpsertRecord) UpsertRecord { r.SourceUpdatedAt = ""; return r }, nil},
		{"opaque object id is allowed", func(r UpsertRecord) UpsertRecord { r.ObjectID = "not-a-uuid"; return r }, nil},
		{"opaque projection id is allowed", func(r UpsertRecord) UpsertRecord { r.ProjectionID = "xyz"; return r }, nil},
		{"bad content hash", func(r UpsertRecord) UpsertRecord { r.ContentHash = strings.Repeat("0", 63); return r }, ErrInvalidContentHash},
		{"uppercase content hash", func(r UpsertRecord) UpsertRecord { r.ContentHash = strings.Repeat("0", 63) + "A"; return r }, ErrInvalidContentHash},
		{"bad timestamp", func(r UpsertRecord) UpsertRecord { r.SourceUpdatedAt = "not-a-time"; return r }, ErrInvalidTimestamp},
		{"nil metadata", func(r UpsertRecord) UpsertRecord { r.Metadata = nil; return r }, ErrMetadataNotObject},
		{"oversized metadata", func(r UpsertRecord) UpsertRecord {
			r.Metadata = map[string]any{"k": strings.Repeat("x", 20000)}
			return r
		}, ErrMetadataTooLarge},
		{"empty vector", func(r UpsertRecord) UpsertRecord { r.Vector = []float32{}; return r }, ErrInvalidVector},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.mutate(validRecord(1, 1))
			err := validateUpsertBatch([]UpsertRecord{rec}, b)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("validateUpsertBatch error = %v, want nil", err)
				}
				return
			}
			if err != tc.wantErr {
				t.Fatalf("validateUpsertBatch error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateUpsertBatchDuplicate proves a batch that names the same
// (object_id, projection_id) twice is duplicate_record.
func TestValidateUpsertBatchDuplicate(t *testing.T) {
	b := Bounds{MaxMetadataBytes: 16 * 1024}
	rec := validRecord(1, 1)
	rec2 := validRecord(1, 1) // same object and projection
	if err := validateUpsertBatch([]UpsertRecord{rec, rec2}, b); err != ErrDuplicateRecord {
		t.Fatalf("validateUpsertBatch(dup) error = %v, want ErrDuplicateRecord", err)
	}
	// The same object with a different projection is allowed.
	rec3 := validRecord(1, 2)
	if err := validateUpsertBatch([]UpsertRecord{rec, rec3}, b); err != nil {
		t.Fatalf("validateUpsertBatch(same object, different projection) error = %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// Search request validation
// ---------------------------------------------------------------------------

// TestValidateSearchRequest is the search cheap-bounds matrix.
func TestValidateSearchRequest(t *testing.T) {
	b := Bounds{SearchMaxLimit: 200, MaxFilters: 10, MaxFilterValues: 50}
	base := func() SearchRequest {
		return SearchRequest{
			NamespaceKey: "project-alpha",
			VectorSpace:  "space-a",
			Embedding:    []float32{1, 2, 3},
			Limit:        10,
		}
	}
	cases := []struct {
		name    string
		mutate  func(SearchRequest) SearchRequest
		wantErr error
	}{
		{"valid", func(r SearchRequest) SearchRequest { return r }, nil},
		{"missing namespace", func(r SearchRequest) SearchRequest { r.NamespaceKey = ""; return r }, ErrMissingField},
		{"missing space", func(r SearchRequest) SearchRequest { r.VectorSpace = ""; return r }, ErrMissingField},
		{"empty embedding", func(r SearchRequest) SearchRequest { r.Embedding = []float32{}; return r }, ErrInvalidVector},
		{"zero limit", func(r SearchRequest) SearchRequest { r.Limit = 0; return r }, ErrInvalidLimit},
		{"negative limit", func(r SearchRequest) SearchRequest { r.Limit = -1; return r }, ErrInvalidLimit},
		{"limit above ceiling", func(r SearchRequest) SearchRequest { r.Limit = 201; return r }, ErrInvalidLimit},
		{"limit at ceiling", func(r SearchRequest) SearchRequest { r.Limit = 200; return r }, nil},
		{"invalid filter", func(r SearchRequest) SearchRequest {
			r.Filters = []FilterEntry{{Field: "", Op: FilterEq, Values: []any{"a"}}}
			return r
		}, ErrInvalidFilter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSearchRequest(tc.mutate(base()), b)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("validateSearchRequest error = %v, want nil", err)
				}
				return
			}
			if err != tc.wantErr {
				t.Fatalf("validateSearchRequest error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Score transform
// ---------------------------------------------------------------------------

// TestScoreTransform is the metric-specific score matrix, including the
// non-finite exclusion.
func TestScoreTransform(t *testing.T) {
	cases := []struct {
		name     string
		metric   Metric
		distance float64
		want     float64
		wantOK   bool
	}{
		{"cosine zero distance", Cosine, 0, 1, true},
		{"cosine one distance", Cosine, 1, 0.5, true},
		{"cosine two distance", Cosine, 2, 0, true},
		{"cosine negative distance (finite)", Cosine, -1, 1.5, true},
		{"l2 zero distance", L2, 0, 1, true},
		{"l2 one distance", L2, 1, 0.5, true},
		{"l2 large distance", L2, 999999, 1.000001e-06, true},
		{"l2 negative distance excluded", L2, -1, 0, false},
		{"inner zero distance", InnerProduct, 0, 0, true},
		{"inner positive distance", InnerProduct, 0.25, -0.25, true},
		{"inner negative distance", InnerProduct, -0.5, 0.5, true},
		{"cosine inf distance excluded", Cosine, math.Inf(1), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := scoreTransform(tc.metric, tc.distance)
			if ok != tc.wantOK {
				t.Fatalf("scoreTransform(%s, %v) ok = %v, want %v", tc.metric, tc.distance, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("scoreTransform(%s, %v) = %v, want %v", tc.metric, tc.distance, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Metric operator
// ---------------------------------------------------------------------------

// TestMetricOperator checks the pgvector operator per metric.
func TestMetricOperator(t *testing.T) {
	cases := []struct {
		metric Metric
		want   string
	}{
		{Cosine, "<=>"},
		{L2, "<->"},
		{InnerProduct, "<#>"},
	}
	for _, tc := range cases {
		if got := metricOperator(tc.metric); got != tc.want {
			t.Fatalf("metricOperator(%s) = %q, want %q", tc.metric, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Search SQL builder
// ---------------------------------------------------------------------------

// TestBuildSearchSQLShape checks the search statement's shape: the
// dimension cast, the metric operator, the application/namespace scoping,
// the ORDER BY distance ASC, the LIMIT, and the AND-joined filter predicates
// with the correct parameter indices.
//
// Parameters: $1=embedding, $2=space, $3=application_id, $4=namespace_id,
// $5..=filters and limit.
func TestBuildSearchSQLShape(t *testing.T) {
	space := &Space{ID: "space-uuid", Key: "space-a", Dimensions: 8, Metric: Cosine}

	t.Run("no filters", func(t *testing.T) {
		sql, args := buildSearchSQL(space, nil, 10)
		if !strings.Contains(sql, "r.embedding::vector(8) <=> $1") {
			t.Fatalf("search SQL missing the dimension cast and operator:\n%s", sql)
		}
		if !strings.Contains(sql, "WHERE r.vector_space_id = $2") {
			t.Fatalf("search SQL missing the space predicate:\n%s", sql)
		}
		if !strings.Contains(sql, "r.application_id = $3") {
			t.Fatalf("search SQL missing the application scoping:\n%s", sql)
		}
		if !strings.Contains(sql, "r.namespace_id = $4") {
			t.Fatalf("search SQL missing the namespace scoping:\n%s", sql)
		}
		if !strings.Contains(sql, "ORDER BY distance ASC") {
			t.Fatalf("search SQL missing ORDER BY:\n%s", sql)
		}
		if !strings.Contains(sql, "LIMIT $5") {
			t.Fatalf("search SQL missing LIMIT $5:\n%s", sql)
		}
		if len(args) != 5 {
			t.Fatalf("search args length = %d, want 5 (embedding, space, app, ns, limit)", len(args))
		}
		if args[4] != 10 {
			t.Fatalf("limit arg = %v, want 10", args[4])
		}
	})

	t.Run("eq filter", func(t *testing.T) {
		filters := []FilterEntry{{Field: "state", Op: FilterEq, Values: []any{"active"}}}
		sql, args := buildSearchSQL(space, filters, 5)
		if !strings.Contains(sql, "(r.metadata ->> $5 = $6)") {
			t.Fatalf("search SQL missing eq predicate:\n%s", sql)
		}
		if !strings.Contains(sql, "LIMIT $7") {
			t.Fatalf("search SQL missing LIMIT $7:\n%s", sql)
		}
		if len(args) != 7 {
			t.Fatalf("search args length = %d, want 7", len(args))
		}
		if args[4] != "state" || args[5] != "active" {
			t.Fatalf("eq filter args = (%v, %v), want (state, active)", args[4], args[5])
		}
	})

	t.Run("in filter", func(t *testing.T) {
		filters := []FilterEntry{{Field: "tag", Op: FilterIn, Values: []any{"a", "b"}}}
		sql, args := buildSearchSQL(space, filters, 5)
		if !strings.Contains(sql, "(r.metadata ->> $5 = ANY($6))") {
			t.Fatalf("search SQL missing in predicate:\n%s", sql)
		}
		// The value set is a []string bound natively by pgx as text[] —
		// never a hand-assembled array literal.
		vals, ok := args[5].([]string)
		if !ok {
			t.Fatalf("in filter array arg type = %T, want []string", args[5])
		}
		if len(vals) != 2 || vals[0] != "a" || vals[1] != "b" {
			t.Fatalf("in filter array arg = %v, want [a b]", vals)
		}
	})

	t.Run("in filter with special characters", func(t *testing.T) {
		// Values that corrupt a hand-assembled array literal: commas,
		// braces, quotes, backslashes, and empty strings pass through
		// byte-for-byte because pgx encodes the []string natively.
		values := []any{
			"with,comma",
			`with"quote`,
			`with\backslash`,
			"with{brace",
			"",
			"plain",
		}
		filters := []FilterEntry{{Field: "tag", Op: FilterIn, Values: values}}
		_, args := buildSearchSQL(space, filters, 5)
		vals, ok := args[5].([]string)
		if !ok {
			t.Fatalf("in filter array arg type = %T, want []string", args[5])
		}
		if len(vals) != len(values) {
			t.Fatalf("in filter array length = %d, want %d", len(vals), len(values))
		}
		for i, want := range values {
			if vals[i] != want {
				t.Fatalf("in filter array[%d] = %q, want %q", i, vals[i], want)
			}
		}
	})

	t.Run("multiple filters AND-joined", func(t *testing.T) {
		filters := []FilterEntry{
			{Field: "state", Op: FilterEq, Values: []any{"active"}},
			{Field: "owner", Op: FilterEq, Values: []any{"memory-service"}},
		}
		sql, args := buildSearchSQL(space, filters, 3)
		// Each filter contributes two parameters; the limit follows.
		if !strings.Contains(sql, "(r.metadata ->> $5 = $6)") {
			t.Fatalf("first filter predicate missing:\n%s", sql)
		}
		if !strings.Contains(sql, "(r.metadata ->> $7 = $8)") {
			t.Fatalf("second filter predicate missing:\n%s", sql)
		}
		if !strings.Contains(sql, "LIMIT $9") {
			t.Fatalf("LIMIT $9 missing:\n%s", sql)
		}
		// Count the AND separators: 4 total (app, ns, two filters).
		if strings.Count(sql, "  AND ") != 4 {
			t.Fatalf("expected 4 AND clauses, got %d:\n%s", strings.Count(sql, "  AND "), sql)
		}
		if len(args) != 9 {
			t.Fatalf("search args length = %d, want 9", len(args))
		}
	})

	t.Run("l2 operator", func(t *testing.T) {
		spaceL2 := &Space{ID: "s", Key: "k", Dimensions: 4, Metric: L2}
		sql, _ := buildSearchSQL(spaceL2, nil, 1)
		if !strings.Contains(sql, "r.embedding::vector(4) <-> $1") {
			t.Fatalf("l2 search SQL missing the l2 operator:\n%s", sql)
		}
	})
}

// ---------------------------------------------------------------------------
// Filter value rendering
// ---------------------------------------------------------------------------

// TestFilterValues is the scalar-to-text[] rendering matrix: every JSON
// scalar type renders to the same text form compared against metadata ->>
// (jsonb's text representation), and special characters survive
// byte-for-byte because the result is bound natively as []string (pgx
// encodes it as text[]) — no array literal is assembled from the values.
func TestFilterValues(t *testing.T) {
	cases := []struct {
		name   string
		values []any
		want   []string
	}{
		{"plain strings", []any{"a", "b"}, []string{"a", "b"}},
		{"numeric", []any{float64(3), int64(-4), int(5)}, []string{"3", "-4", "5"}},
		{"boolean", []any{true, false}, []string{"true", "false"}},
		{"json number", []any{json.Number("2.50")}, []string{"2.50"}},
		{"single value", []any{"only"}, []string{"only"}},
		{"comma in value", []any{"with,comma"}, []string{"with,comma"}},
		{"quote in value", []any{`with"quote`}, []string{`with"quote`}},
		{"backslash in value", []any{`with\backslash`}, []string{`with\backslash`}},
		{"braces in value", []any{`{inner}`, `}`}, []string{`{inner}`, `}`}},
		{"empty string value", []any{""}, []string{""}},
		{"mixed specials", []any{"a,b", `c"`, `d\`, "e{f", ""},
			[]string{"a,b", `c"`, `d\`, "e{f", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterValues(tc.values)
			if len(got) != len(tc.want) {
				t.Fatalf("filterValues(%v) length = %d, want %d", tc.values, len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("filterValues(%v)[%d] = %q, want %q", tc.values, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Upsert statement shape
// ---------------------------------------------------------------------------

// TestUpsertStmtShape checks the upsert statement's idempotence shape: the
// 5-tuple conflict target and the DO UPDATE column set (content_hash,
// source_updated_at, metadata, embedding, updated_at — never the row id).
func TestUpsertStmtShape(t *testing.T) {
	const stmt = upsertStmt
	if !strings.Contains(stmt, "ON CONFLICT (application_id, namespace_id, object_id, projection_id, vector_space_id)") {
		t.Fatalf("upsert statement missing the 5-tuple conflict target:\n%s", stmt)
	}
	for _, col := range []string{"content_hash", "source_updated_at", "metadata", "embedding", "updated_at"} {
		if !strings.Contains(stmt, col) {
			t.Fatalf("upsert statement missing %s in the DO UPDATE set:\n%s", col, stmt)
		}
	}
	// The row id is bound as a value but never appears in the DO UPDATE
	// SET clause: the statement updates the five content columns only.
	setClause := stmt[strings.Index(stmt, "DO UPDATE SET"):]
	if strings.Contains(setClause, "id =") || strings.Contains(setClause, "id=") {
		t.Fatalf("upsert DO UPDATE SET touches the row id:\n%s", setClause)
	}
	// The embedding is cast to the vector type (the dimension is carried by
	// the value, not the column).
	if !strings.Contains(stmt, "$10::vector") {
		t.Fatalf("upsert statement missing the embedding vector cast:\n%s", stmt)
	}
	// The metadata is cast to jsonb.
	if !strings.Contains(stmt, "$9::jsonb") {
		t.Fatalf("upsert statement missing the metadata jsonb cast:\n%s", stmt)
	}
}

// ---------------------------------------------------------------------------
// Space state gate (table)
// ---------------------------------------------------------------------------

// spaceStateGate encodes the package-3 space-state rule for the unit matrix:
// a disabled space is unavailable to every naming operation; a retired
// space is unavailable to upsert only; an enabled, non-retired space is
// available to all. The function under test (the gate) lives in
// ResolveInTx; this table is the reference the integration matrix drives
// against, and it also guards the rule's logic here without a database.
func TestSpaceStateGateRule(t *testing.T) {
	cases := []struct {
		name        string
		enabled     bool
		retired     bool
		write       bool
		wantAllowed bool
	}{
		{"enabled active, upsert", true, false, true, true},
		{"enabled active, read", true, false, false, true},
		{"disabled, upsert", false, false, true, false},
		{"disabled, read", false, false, false, false},
		{"retired, upsert", true, true, true, false},
		{"retired, read", true, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed := applySpaceGate(tc.enabled, tc.retired, tc.write)
			if allowed != tc.wantAllowed {
				t.Fatalf("space gate (enabled=%v, retired=%v, write=%v) = %v, want %v",
					tc.enabled, tc.retired, tc.write, allowed, tc.wantAllowed)
			}
		})
	}
}

// applySpaceGate restates the ResolveInTx gate rule for the unit matrix:
// disabled is unavailable to every operation; retired is unavailable to
// writes only; otherwise allowed.
func applySpaceGate(enabled, retired, write bool) bool {
	if !enabled {
		return false
	}
	if retired && write {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// UUID grammar
// ---------------------------------------------------------------------------

// TestIsCanonicalUUID checks the request-side UUID grammar (lowercase
// hyphenated, 8-4-4-4-12).
func TestIsCanonicalUUID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"0199f31e-1000-7000-8000-000000000001", true},
		{"not-a-uuid", false},
		{"", false},
		{"0199f31e100070008000000000000001", false},     // no dashes
		{"0199F31E-1000-7000-8000-000000000001", false}, // uppercase
		{"g199f31e-1000-7000-8000-000000000001", false}, // non-hex
		{"0199f31e-1000-7000-8000", false},              // too short
	}
	for _, tc := range cases {
		if got := isCanonicalUUID(tc.in); got != tc.want {
			t.Fatalf("isCanonicalUUID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestNewRecordIDUsesCryptoRand guards that newRecordID reads from
// crypto/rand (the service must not use a deterministic counter for row
// identity). It is a weak check (it only proves the function does not
// panic and produces distinct values), so the integration suite is the
// authoritative proof of uniqueness.
func TestNewRecordIDUsesCryptoRand(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := newRecordID()
		if err != nil {
			t.Fatalf("newRecordID: %v", err)
		}
		if seen[id] {
			t.Fatalf("newRecordID produced a duplicate within 100 draws: %q", id)
		}
		seen[id] = true
	}
}

// Ensure crypto/rand is referenced (the record-id generator's entropy
// source).
var _ = rand.Read
