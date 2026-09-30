package migrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

const testFile = "0001_test.sql"

func mustStrip(t *testing.T, file, raw string) []byte {
	t.Helper()
	body, err := stripFraming(file, []byte(raw))
	if err != nil {
		t.Fatalf("stripFraming(%q) unexpected error: %v", raw, err)
	}
	return body
}

// expectStripError requires stripFraming to fail with an error naming the
// file and containing every fragment. Whether the error is a *framingError
// is asserted by the caller.
func expectStripError(t *testing.T, file, raw string, fragments ...string) error {
	t.Helper()
	_, err := stripFraming(file, []byte(raw))
	if err == nil {
		t.Fatalf("stripFraming(%q): expected error, got nil", raw)
	}
	got := err.Error()
	for _, frag := range fragments {
		if !strings.Contains(got, frag) {
			t.Fatalf("stripFraming(%q): error %q does not contain %q", raw, got, frag)
		}
	}
	if !strings.Contains(got, file) {
		t.Fatalf("stripFraming(%q): error %q does not name the file", raw, got)
	}
	return err
}

func TestStripFraming(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string // required error fragment; empty means success
		// nonFramingErr marks scanner-level failures (unterminated
		// constructs) whose error is not a *framingError.
		nonFramingErr bool
		wantBody      string
	}{
		{
			name:     "standard framing",
			input:    "BEGIN;\n\nCREATE TABLE a (id int);\n\nCOMMIT;\n",
			wantBody: "\nCREATE TABLE a (id int);\n\n",
		},
		{
			name:     "framing keywords are case-insensitive",
			input:    "begin;\nCREATE TABLE a (id int);\ncommit;\n",
			wantBody: "CREATE TABLE a (id int);\n",
		},
		{
			name:     "comments and blanks before BEGIN and after COMMIT",
			input:    "-- released migration\n\nBEGIN;\n\nCREATE TABLE a (id int);\nCOMMIT;\n\n-- trailing comment\n",
			wantBody: "-- released migration\n\n\nCREATE TABLE a (id int);\n\n-- trailing comment\n",
		},
		{
			name:     "no trailing newline after COMMIT",
			input:    "BEGIN;\nCREATE TABLE a (id int);\nCOMMIT;",
			wantBody: "CREATE TABLE a (id int);\n",
		},
		{
			name:     "empty body",
			input:    "BEGIN;\nCOMMIT;\n",
			wantBody: "",
		},
		{
			name:     "plpgsql dollar-quoted body with BEGIN and END",
			input:    "BEGIN;\nCREATE FUNCTION f() RETURNS void AS $$\nBEGIN\n    NULL;\nEND;\n$$ LANGUAGE plpgsql;\nCOMMIT;\n",
			wantBody: "CREATE FUNCTION f() RETURNS void AS $$\nBEGIN\n    NULL;\nEND;\n$$ LANGUAGE plpgsql;\n",
		},
		{
			name:     "tagged dollar-quoted body with transaction keywords",
			input:    "BEGIN;\nSELECT $fn$ BEGIN; COMMIT; END; $fn$;\nCOMMIT;\n",
			wantBody: "SELECT $fn$ BEGIN; COMMIT; END; $fn$;\n",
		},
		{
			name:     "single-quoted literal containing transaction keywords",
			input:    "BEGIN;\nINSERT INTO t (m) VALUES ('BEGIN; COMMIT; END; ROLLBACK; ABORT;');\nCOMMIT;\n",
			wantBody: "INSERT INTO t (m) VALUES ('BEGIN; COMMIT; END; ROLLBACK; ABORT;');\n",
		},
		{
			name:     "escaped quote inside single-quoted literal",
			input:    "BEGIN;\nINSERT INTO t (m) VALUES ('it''s a BEGIN; test');\nCOMMIT;\n",
			wantBody: "INSERT INTO t (m) VALUES ('it''s a BEGIN; test');\n",
		},
		{
			name:     "escape string with backslash escapes",
			input:    "BEGIN;\nINSERT INTO t (m) VALUES (E'a\\'BEGIN; b');\nCOMMIT;\n",
			wantBody: "INSERT INTO t (m) VALUES (E'a\\'BEGIN; b');\n",
		},
		{
			name:     "line comment containing a transaction statement",
			input:    "BEGIN;\n-- COMMIT;\nCREATE TABLE a (id int);\nCOMMIT;\n",
			wantBody: "-- COMMIT;\nCREATE TABLE a (id int);\n",
		},
		{
			name:     "block comment containing a transaction statement",
			input:    "BEGIN;\n/* COMMIT;\n   BEGIN; */\nCREATE TABLE a (id int);\nCOMMIT;\n",
			wantBody: "/* COMMIT;\n   BEGIN; */\nCREATE TABLE a (id int);\n",
		},
		{
			name:     "nested block comment",
			input:    "BEGIN;\n/* outer /* inner COMMIT; */ still comment */\nCREATE TABLE a (id int);\nCOMMIT;\n",
			wantBody: "/* outer /* inner COMMIT; */ still comment */\nCREATE TABLE a (id int);\n",
		},
		{
			name:    "missing leading BEGIN",
			input:   "CREATE TABLE a (id int);\nCOMMIT;\n",
			wantErr: clauseBegin,
		},
		{
			name:    "missing trailing COMMIT",
			input:   "BEGIN;\nCREATE TABLE a (id int);\n",
			wantErr: clauseCommit,
		},
		{
			name:    "BEGIN without semicolon",
			input:   "BEGIN\nCREATE TABLE a (id int);\nCOMMIT;\n",
			wantErr: clauseBegin,
		},
		{
			name:    "COMMIT without semicolon",
			input:   "BEGIN;\nCREATE TABLE a (id int);\nCOMMIT",
			wantErr: clauseCommit,
		},
		{
			name:    "BEGIN with transaction mode is not exactly BEGIN;",
			input:   "BEGIN WORK;\nCREATE TABLE a (id int);\nCOMMIT;\n",
			wantErr: clauseBegin,
		},
		{
			name:    "blank and comment lines only",
			input:   "\n-- nothing here\n\n",
			wantErr: clauseBegin,
		},
		{
			name:    "empty file",
			input:   "",
			wantErr: clauseBegin,
		},
		{
			name:    "mid-body top-level COMMIT",
			input:   "BEGIN;\nCREATE TABLE a (id int);\nCOMMIT;\nCREATE TABLE b (id int);\nCOMMIT;\n",
			wantErr: clauseControl,
		},
		{
			name:    "mid-body top-level BEGIN",
			input:   "BEGIN;\nCREATE TABLE a (id int);\nBEGIN;\nCOMMIT;\n",
			wantErr: clauseControl,
		},
		{
			name:    "mid-body top-level END",
			input:   "BEGIN;\nEND;\nCOMMIT;\n",
			wantErr: clauseControl,
		},
		{
			name:    "mid-body top-level ROLLBACK",
			input:   "BEGIN;\nROLLBACK;\nCOMMIT;\n",
			wantErr: clauseControl,
		},
		{
			name:    "mid-body top-level ABORT",
			input:   "BEGIN;\nABORT;\nCOMMIT;\n",
			wantErr: clauseControl,
		},
		{
			name:    "mid-body top-level START TRANSACTION",
			input:   "BEGIN;\nSTART TRANSACTION;\nCOMMIT;\n",
			wantErr: clauseControl,
		},
		{
			name:    "mid-body top-level START WORK",
			input:   "BEGIN;\nSTART WORK;\nCOMMIT;\n",
			wantErr: clauseControl,
		},
		{
			name:    "lowercase mid-body commit is still transaction control",
			input:   "BEGIN;\ncreate table a (id int);\ncommit;\nCOMMIT;\n",
			wantErr: clauseControl,
		},
		{
			name:          "unterminated string literal is fatal",
			input:         "BEGIN;\nSELECT 'unterminated;\nCOMMIT;\n",
			wantErr:       "unterminated string literal",
			nonFramingErr: true,
		},
		{
			name:          "unterminated dollar-quoted body is fatal",
			input:         "BEGIN;\nSELECT $$unterminated;\nCOMMIT;\n",
			wantErr:       "unterminated dollar-quoted body",
			nonFramingErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantErr != "" {
				err := expectStripError(t, testFile, tc.input, tc.wantErr)
				if !tc.nonFramingErr {
					var fe *framingError
					if !errors.As(err, &fe) {
						t.Fatalf("expected *framingError, got %T (%v)", err, err)
					}
				}
				return
			}
			body := mustStrip(t, testFile, tc.input)
			if string(body) != tc.wantBody {
				t.Fatalf("execution stream mismatch:\n got %q\nwant %q", body, tc.wantBody)
			}
		})
	}
}

// TestStripFramingExecutionStreamIsOriginalMinusFramingLines verifies, for
// every embedded file, that the execution stream equals the original bytes
// minus exactly the outer BEGIN;/COMMIT; framing lines, computed here
// independently of stripFraming's line walking.
func TestStripFramingExecutionStreamIsOriginalMinusFramingLines(t *testing.T) {
	files, err := embeddedSource{}.migrations()
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	for _, m := range files {
		raw := m.raw
		firstNL := bytes.IndexByte(raw, '\n')
		if firstNL < 0 {
			t.Fatalf("%s: no newline", m.name)
		}
		firstLineEnd := firstNL + 1

		// Start of the last physical line: the first byte after the last
		// newline that precedes at least one non-newline byte.
		lastLineStart := len(raw)
		for i := len(raw) - 1; i >= 0; i-- {
			if raw[i] != '\n' {
				lastLineStart = i
				break
			}
		}
		for lastLineStart > 0 && raw[lastLineStart-1] != '\n' {
			lastLineStart--
		}

		expected := raw[firstLineEnd:lastLineStart]
		if !bytes.Equal(m.body, expected) {
			t.Errorf("%s: execution stream is not the original minus exactly the two framing lines", m.name)
		}
		// The framing lines themselves are the outer BEGIN;/COMMIT; lines.
		if !isFramingKeyword(raw[:firstLineEnd], "BEGIN") {
			t.Errorf("%s: first line %q is not a BEGIN; line", m.name, raw[:firstLineEnd])
		}
		if !isFramingKeyword(raw[lastLineStart:], "COMMIT") {
			t.Errorf("%s: last line %q is not a COMMIT; line", m.name, raw[lastLineStart:])
		}
	}
}

func TestChecksum(t *testing.T) {
	raw := []byte("BEGIN;\nSELECT 1;\nCOMMIT;\n")
	got := checksum(raw)

	sum := sha256.Sum256(raw)
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("checksum mismatch:\n got %s\nwant %s (SHA-256 of original bytes)", got, want)
	}

	// The checksum is over the original bytes, not the stripped body.
	body := mustStrip(t, testFile, string(raw))
	if checksum(body) == got {
		t.Fatalf("checksum of stripped body must differ from checksum of original bytes")
	}
}

// publishedChecksums are the SHA-256 values recorded in migrations/README.md
// for the released migration files.
var publishedChecksums = map[string]string{
	"0001_shared_vector_schema.sql":                  "fd7c7dc08ac93a759621a4504023506c4f11e22009a33a6a76b7fa6544967fbc",
	"0002_qwen3_embedding_space.sql":                 "de65b429ca983e24990f95499ae96dced9c34cf2e02786b860aa065350c70060",
	"0003_qwen3_hnsq.sql":                            "730b80c77ab232ca31dacad704df579241d74c54a79a870e02f29dc48f640b51",
	"0004_runtime_identity.sql":                      "bc13c6618de1a30fb80bb81db2426052e4e39e2316a596c606eaf5aa9508a21f",
	"0005_runtime_privilege_boundary.sql":            "e7097c444a650855773ccad87f1677d4d37e9f6935014a9745d2eb0fb4b20490",
	"0006_runtime_privilege_boundary_completion.sql": "29bd9e54d5da131cf9eeefab674cef2c8c829b76bbe74264f7f9dff4c5b7dfb9",
}

func TestEmbeddedChecksumsMatchPublishedContract(t *testing.T) {
	files, err := embeddedSource{}.migrations()
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	if len(files) != len(publishedChecksums) {
		t.Fatalf("embedded migration count = %d, want %d", len(files), len(publishedChecksums))
	}
	for _, m := range files {
		want, ok := publishedChecksums[m.name]
		if !ok {
			t.Fatalf("embedded file %s is not in the published checksum table", m.name)
		}
		if m.checksum != want {
			t.Errorf("migration %s checksum = %s, published contract value = %s", m.name, m.checksum, want)
		}
	}
}

func TestParseFileName(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		wantVer int
		wantSuf string
		wantOK  bool
	}{
		{name: "canonical", file: "0001_shared_vector_schema.sql", wantVer: 1, wantSuf: "shared_vector_schema", wantOK: true},
		{name: "five digits", file: "12345_later.sql", wantVer: 12345, wantSuf: "later", wantOK: true},
		{name: "three digits rejected", file: "001_short.sql", wantOK: false},
		{name: "no prefix", file: "no_prefix.sql", wantOK: false},
		{name: "non-numeric prefix", file: "abcd_name.sql", wantOK: false},
		{name: "wrong extension", file: "0001_name.txt", wantOK: false},
		{name: "no extension", file: "0001_name", wantOK: false},
		{name: "empty suffix", file: "0001_.sql", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version, suffix, ok := parseFileName(tc.file)
			if ok != tc.wantOK {
				t.Fatalf("parseFileName(%q) ok = %v, want %v", tc.file, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if version != tc.wantVer || suffix != tc.wantSuf {
				t.Fatalf("parseFileName(%q) = (%d, %q), want (%d, %q)", tc.file, version, suffix, tc.wantVer, tc.wantSuf)
			}
		})
	}
}
