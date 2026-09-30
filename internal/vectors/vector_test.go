package vectors

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

// The unit matrix of the shared vector-validation core (package 3, section
// 2; no database, table-driven): the structural validation (JSON array of
// numbers), the finite/float32-representable rule, the dimension check
// against a resolved dimensionality, the deterministic zero-norm rule per
// metric, and the pgvector text codec's round-trip form. The real-PostgreSQL
// enforcement (the trigger's P0001 rejections, the non-finite-score
// exclusion, the space-state matrix through the API) is integration
// territory and is never mocked here.
//
// Numeric inputs are produced by json.Marshal of a []float64: that is the
// exact encoding the package-4 strict-JSON helper produces when it decodes
// the vector field, so the matrix exercises the production shape of the
// bytes ParseVector receives (no non-standard JSON tokens such as NaN or
// Infinity literals ever reach it).

func numJSON(vals ...float64) []byte {
	b, err := json.Marshal(vals)
	if err != nil {
		panic(fmt.Sprintf("vectors test: marshal fixture: %v", err))
	}
	return b
}

func TestParseVectorStructure(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr error // nil when the input is structurally valid
		want    []float32
	}{
		{"valid simple", "[1,2,3]", nil, []float32{1, 2, 3}},
		{"valid floats", "[0.5,-0.25,3.14]", nil, []float32{0.5, -0.25, 3.14}},
		{"valid exponent", "[1e-3,2.5e2]", nil, []float32{1e-3, 250}},
		{"empty array is structural", "[]", nil, []float32{}},
		{"not an array: number", "1", ErrInvalidVector, nil},
		{"not an array: string", "\"abc\"", ErrInvalidVector, nil},
		{"not an array: object", "{\"a\":1}", ErrInvalidVector, nil},
		{"not an array: nested array", "[[1]]", ErrInvalidVector, nil},
		{"mixed types: string element", "[1,\"x\"]", ErrInvalidVector, nil},
		{"mixed types: object element", "[1,{\"a\":1}]", ErrInvalidVector, nil},
		{"bool element", "[true]", ErrInvalidVector, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseVector([]byte(tc.raw))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseVector(%s) error = %v, want %v", tc.raw, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseVector(%s) unexpected error: %v", tc.raw, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseVector(%s) length = %d, want %d", tc.raw, len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ParseVector(%s)[%d] = %v, want %v", tc.raw, i, got[i], tc.want[i])
				}
			}
		})
	}

	// JSON null is not a number: the bare-document null case yields a
	// *json.UnmarshalTypeError (top-level type mismatch), and the in-array
	// element case likewise. Both are the invalid-vector shape (400),
	// never a silent zero.
	for name, raw := range map[string]string{
		"bare null":    "null",
		"null element": `[1,null]`,
	} {
		t.Run("not a number: "+name, func(t *testing.T) {
			_, err := ParseVector([]byte(raw))
			if !errors.Is(err, ErrInvalidVector) {
				t.Fatalf("ParseVector(%s) error = %v, want ErrInvalidVector", raw, err)
			}
		})
	}

	// Malformed-JSON cases: json.Unmarshal surfaces the *json.SyntaxError,
	// which ParseVector passes through (strict decoding is the package-4
	// handler's responsibility, which maps it to invalid_json).
	for _, bad := range []string{`[1,`, `[1]x`, `{`, ``, `1 2`} {
		_, err := ParseVector([]byte(bad))
		var syn *json.SyntaxError
		if !errors.As(err, &syn) {
			t.Fatalf("ParseVector(%q) error = %v (%T), want *json.SyntaxError", bad, err, err)
		}
	}
}

// TestParseVectorFinite covers the finite rule and the float32
// representability rule: a finite float64 input whose magnitude overflows
// the float32 range (the conversion rounds it to ±Inf) yields
// ErrNonFiniteVector. NaN and ±Infinity can never appear in standard JSON
// (the Go decoder rejects the tokens at the syntax level), so the
// representability overflow is the only in-band source of the error.
func TestParseVectorFinite(t *testing.T) {
	cases := []struct {
		name string
		vals []float64
	}{
		{"float64 overflow positive", []float64{1e40}},
		{"float64 overflow negative", []float64{-1e40}},
		{"overflow among valid", []float64{1, 1e40, 2}},
		{"overflow just above float32 max", []float64{3.4028236e38}},
		{"overflow in last position", []float64{1, 2, 3.4028236e38}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := numJSON(tc.vals...)
			_, err := ParseVector(raw)
			if !errors.Is(err, ErrNonFiniteVector) {
				t.Fatalf("ParseVector(%s) error = %v, want ErrNonFiniteVector", raw, err)
			}
		})
	}

	// The boundary: float32 max itself is representable and accepted.
	raw := numJSON(float64(math.MaxFloat32))
	got, err := ParseVector(raw)
	if err != nil {
		t.Fatalf("ParseVector(%s) error = %v, want nil", raw, err)
	}
	if got[0] != math.MaxFloat32 {
		t.Fatalf("ParseVector = %v, want %v", got[0], math.MaxFloat32)
	}
	// The smallest positive subnormal is representable (as a subnormal)
	// and accepted — the rule rejects non-finite values only.
	raw = numJSON(5e-324)
	if _, err := ParseVector(raw); err != nil {
		t.Fatalf("ParseVector(%s) error = %v, want nil (subnormal is finite and representable)", raw, err)
	}
}

// TestParseVectorFloat32Precision pins the conversion target: every accepted
// element is the float32 rounding of the input float64.
func TestParseVectorFloat32Precision(t *testing.T) {
	cases := []struct {
		name string
		vals []float64
		want []float32
	}{
		{"exact ints", []float64{0, 1, -1, 4294967295}, []float32{0, 1, -1, 4294967295}},
		{"float32 max representable", []float64{float64(math.MaxFloat32)}, []float32{math.MaxFloat32}},
		{"tiny values", []float64{1e-38, -1e-38}, []float32{1e-38, -1e-38}},
		{"half values", []float64{0.5, -0.5, 0.25}, []float32{0.5, -0.5, 0.25}},
		{"float64 rounds to float32", []float64{0.1 + 1e-18}, []float32{float32(0.1 + 1e-18)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseVector(numJSON(tc.vals...))
			if err != nil {
				t.Fatalf("ParseVector error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("length = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestCheckDimensions is the dimension check against a resolved
// dimensionality: wrong lengths fail, matching lengths pass, and the empty
// vector fails against every real dimensionality (the structural path).
func TestCheckDimensions(t *testing.T) {
	cases := []struct {
		name    string
		v       []float32
		dim     int
		wantErr bool
	}{
		{"match", []float32{1, 2, 3}, 3, false},
		{"short", []float32{1}, 3, true},
		{"long", []float32{1, 2, 3, 4}, 3, true},
		{"empty against zero dims", []float32{}, 0, false},
		{"empty against resolved dims", []float32{}, 1024, true},
		{"single dim", []float32{0.5}, 1, false},
		{"1024 dims", make([]float32, 1024), 1024, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckDimensions(tc.v, tc.dim)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckDimensions error = %v, want error = %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidDimensions) {
				t.Fatalf("CheckDimensions error = %v, want ErrInvalidDimensions", err)
			}
		})
	}
}

// TestIsZero is the deterministic zero-norm test: every element exactly
// 0.0 (no epsilon, no norm computation). A single non-zero element in any
// position defeats the rule.
func TestIsZero(t *testing.T) {
	cases := []struct {
		name string
		v    []float32
		want bool
	}{
		{"all zero", make([]float32, 5), true},
		{"empty is vacuously zero", []float32{}, true},
		{"single non-zero first", []float32{1, 0, 0}, false},
		{"single non-zero middle", []float32{0, 1, 0}, false},
		{"single non-zero last", []float32{0, 0, 1}, false},
		{"tiny non-zero", []float32{0, 1e-30}, false},
		{"negative zero counts as zero", []float32{0, -0}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsZero(tc.v); got != tc.want {
				t.Fatalf("IsZero = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCheckZeroNorm is the metric gate: the all-zero vector is rejected in
// a cosine space (both for the upsert vectors and the search query — the
// check does not distinguish), and accepted unchanged in l2 and
// inner_product spaces. A non-zero vector is accepted in every metric.
func TestCheckZeroNorm(t *testing.T) {
	allZero := make([]float32, 3)
	nonZero := []float32{0, 0, 1}
	cases := []struct {
		name    string
		v       []float32
		metric  Metric
		wantErr bool
	}{
		{"cosine all zero", allZero, Cosine, true},
		{"cosine non-zero", nonZero, Cosine, false},
		{"l2 all zero", allZero, L2, false},
		{"l2 non-zero", nonZero, L2, false},
		{"inner_product all zero", allZero, InnerProduct, false},
		{"inner_product non-zero", nonZero, InnerProduct, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			space := Space{Key: "fixture-space", Dimensions: 3, Metric: tc.metric}
			err := CheckZeroNorm(tc.v, space)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckZeroNorm error = %v, want error = %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrNonFiniteVector) {
				t.Fatalf("CheckZeroNorm error = %v, want ErrNonFiniteVector", err)
			}
		})
	}
}

// TestCheckZeroNormSingleNonZeroDefeatsRule covers the contract point
// explicitly: a single non-zero element in ANY position defeats the rule in
// a cosine space.
func TestCheckZeroNormSingleNonZeroDefeatsRule(t *testing.T) {
	for pos := 0; pos < 1024; pos += 255 { // first, middle, last, ...
		v := make([]float32, 1024)
		v[pos] = 1
		err := CheckZeroNorm(v, Space{Key: "k", Dimensions: 1024, Metric: Cosine})
		if err != nil {
			t.Fatalf("position %d: CheckZeroNorm = %v, want nil", pos, err)
		}
	}
}

// TestEncodeTextRoundTrip is the codec's round-trip form: the text form
// "[a,b,c,...]" parses back (as float32) to the identical bit pattern for a
// corpus including the edge values, and the rendered string never contains
// a non-finite token.
func TestEncodeTextRoundTrip(t *testing.T) {
	values := []float32{
		0, -0, 1, -1, 0.5, -0.5, 0.25,
		1e-38, -1e-38,
		math.MaxFloat32,
		-math.MaxFloat32,
		1.17549435e-38,              // float32 min positive normal
		math.SmallestNonzeroFloat32, // denormal
		1.5, 2.5, 0.1, -0.1,
		123456.75,
		0.000123456,
		1.152921504606847e18, // 2^60
	}
	s := EncodeText(values)
	// Structural form: [a,b,c]
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		t.Fatalf("EncodeText = %q, want [...] form", s)
	}
	if strings.Contains(s, "nan") || strings.Contains(s, "inf") {
		t.Fatalf("EncodeText rendered a non-finite token: %q", s)
	}
	// Round-trip: decode the rendered numbers and compare bit patterns.
	inner := s[1 : len(s)-1]
	var parts []string
	if inner != "" {
		parts = strings.Split(inner, ",")
	}
	if len(parts) != len(values) {
		t.Fatalf("EncodeText has %d components, want %d", len(parts), len(values))
	}
	for i, p := range parts {
		var f float64
		if _, err := fmt.Sscanf(p, "%g", &f); err != nil {
			t.Fatalf("component %d %q does not parse: %v", i, p, err)
		}
		back := float32(f)
		if math.Float32bits(back) != math.Float32bits(values[i]) {
			t.Fatalf("component %d: %q round-trips to %v (%08x), want %v (%08x)",
				i, p, back, math.Float32bits(back), values[i], math.Float32bits(values[i]))
		}
	}
}

// TestEncodeTextKnownForm pins concrete renderings (shortest round-trip
// form, comma-separated, no spaces). The expectations are computed with the
// same %g precision -1 the codec uses, so the assertions pin the FORMAT
// (wrapping, separators, absence of rounding artifacts), not a table of
// decimal literals.
func TestEncodeTextKnownForm(t *testing.T) {
	cases := []struct {
		name string
		v    []float32
	}{
		{"integers", []float32{1, 2, 3}},
		{"negatives", []float32{0.5, -0.25}},
		{"small decimal", []float32{float32(1e-3)}},
		{"zero", []float32{0}},
		{"empty", []float32{}},
		{"large exponent", []float32{1e30}},
		{"large integer-ish", []float32{123456.75}},
		{"float32 max", []float32{math.MaxFloat32}},
		{"negative zero", []float32{-0}},
		{"signs", []float32{-1, -2, -3}},
		{"normals", []float32{1.17549435e-38, 2.0, 3.0}},
		{"subnormals", []float32{math.SmallestNonzeroFloat32, 1e-39, 1e-40}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want strings.Builder
			want.WriteByte('[')
			for i, c := range tc.v {
				if i > 0 {
					want.WriteByte(',')
				}
				want.WriteString(strconv.FormatFloat(float64(c), 'g', -1, 32))
			}
			want.WriteByte(']')
			if got := EncodeText(tc.v); got != want.String() {
				t.Fatalf("EncodeText = %q, want %q", got, want.String())
			}
		})
	}
}

// TestEncodeTextNoSpaces pins the byte-exact separator form for a short
// vector (no whitespace, no trailing comma): the pgvector text grammar is
// exact and the binding must not introduce spaces.
func TestEncodeTextNoSpaces(t *testing.T) {
	if got := EncodeText([]float32{1, -2, 3}); got != "[1,-2,3]" {
		t.Fatalf("EncodeText = %q, want %q", got, "[1,-2,3]")
	}
	if got := EncodeText([]float32{}); got != "[]" {
		t.Fatalf("EncodeText(empty) = %q, want %q", got, "[]")
	}
}

// TestParseVectorRoundTripThroughCodec is the end-to-end codec contract:
// accepted JSON numbers → ParseVector → EncodeText → decoded float32 are
// bit-identical to ParseVector's output. This is the property the data
// plane relies on when it binds the encoded text: the database stores the
// caller's values at full float32 precision.
func TestParseVectorRoundTripThroughCodec(t *testing.T) {
	cases := [][]float64{
		{1, 2, 3},
		{0.5, -0.25, 0.1},
		{1e-38, -1e-38, math.SmallestNonzeroFloat32},
		{math.MaxFloat32, -math.MaxFloat32},
		{0.000123456, 123456.75},
	}
	for i, vals := range cases {
		v, err := ParseVector(numJSON(vals...))
		if err != nil {
			t.Fatalf("case %d: ParseVector: %v", i, err)
		}
		s := EncodeText(v)
		parts := strings.Split(s[1:len(s)-1], ",")
		if len(parts) != len(v) {
			t.Fatalf("case %d: %d components, want %d", i, len(parts), len(v))
		}
		for j, p := range parts {
			var f float64
			fmt.Sscanf(p, "%g", &f)
			if math.Float32bits(float32(f)) != math.Float32bits(v[j]) {
				t.Fatalf("case %d component %d: %q does not round-trip", i, j, p)
			}
		}
	}
}
