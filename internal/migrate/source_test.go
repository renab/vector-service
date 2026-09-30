package migrate

import (
	"strings"
	"testing"
)

func TestEmbeddedSourceOrderAndIdentity(t *testing.T) {
	files, err := embeddedSource{}.migrations()
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	if len(files) != 6 {
		t.Fatalf("embedded migration count = %d, want 6", len(files))
	}
	for i, m := range files {
		if m.version != i+1 {
			t.Errorf("migration[%d] version = %d, want %d", i, m.version, i+1)
		}
		if m.name == "" || !strings.HasSuffix(m.name, ".sql") {
			t.Errorf("migration[%d] name %q is not a .sql basename", i, m.name)
		}
		if m.checksum == "" {
			t.Errorf("migration %s has empty checksum", m.name)
		}
		// The execution stream must never contain the outer framing as
		// top-level statements: the body is what a runner-owned
		// transaction executes verbatim.
		if strings.HasPrefix(string(m.body), "BEGIN") || strings.Contains(string(m.body), "\nCOMMIT;\n") {
			t.Errorf("migration %s execution stream still carries outer framing", m.name)
		}
	}
}

func TestSyntheticSourceRejectsMalformedFraming(t *testing.T) {
	src := newSyntheticSource(map[string][]byte{
		"0001_ok.sql":      []byte("BEGIN;\nCREATE TABLE a (id int);\nCOMMIT;\n"),
		"0002_bad.sql":     []byte("BEGIN;\nCREATE TABLE b (id int);\nCOMMIT;\nCOMMIT;\n"),
		"0003_missing.sql": []byte("CREATE TABLE c (id int);\nCOMMIT;\n"),
	})
	_, err := src.migrations()
	if err == nil {
		t.Fatal("expected malformed framing to be fatal")
	}
	if !strings.Contains(err.Error(), "0002_bad.sql") {
		t.Fatalf("error should name the malformed file, got: %v", err)
	}
}

func TestSyntheticSourceRejectsDuplicateVersions(t *testing.T) {
	src := newSyntheticSource(map[string][]byte{
		"0001_first.sql":  []byte("BEGIN;\nSELECT 1;\nCOMMIT;\n"),
		"0001_second.sql": []byte("BEGIN;\nSELECT 2;\nCOMMIT;\n"),
	})
	_, err := src.migrations()
	if err == nil {
		t.Fatal("expected duplicate version to be fatal")
	}
	if !strings.Contains(err.Error(), "duplicate migration version 1") {
		t.Fatalf("error should name the duplicate version, got: %v", err)
	}
}

func TestSyntheticSourceRejectsBadFileName(t *testing.T) {
	src := newSyntheticSource(map[string][]byte{
		"no_prefix.sql": []byte("BEGIN;\nSELECT 1;\nCOMMIT;\n"),
	})
	_, err := src.migrations()
	if err == nil {
		t.Fatal("expected non-conforming filename to be fatal")
	}
	if !strings.Contains(err.Error(), "naming convention") {
		t.Fatalf("error should cite the naming convention, got: %v", err)
	}
}

func TestSyntheticSourceAppliesFramingStrippingAndChecksum(t *testing.T) {
	raw := []byte("BEGIN;\n\nCREATE TABLE fixture (id int);\n\nCOMMIT;\n")
	src := newSyntheticSource(map[string][]byte{
		"0001_fixture.sql": raw,
	})
	files, err := src.migrations()
	if err != nil {
		t.Fatalf("load synthetic migrations: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("synthetic migration count = %d, want 1", len(files))
	}
	m := files[0]
	if m.version != 1 || m.name != "0001_fixture.sql" {
		t.Fatalf("migration identity = (%d, %q), want (1, %q)", m.version, m.name, "0001_fixture.sql")
	}
	if string(m.body) != "\nCREATE TABLE fixture (id int);\n\n" {
		t.Fatalf("execution stream = %q, want the body minus exactly the framing lines", m.body)
	}
	if m.checksum != checksum(raw) {
		t.Fatal("checksum must be computed over the original bytes")
	}
}
