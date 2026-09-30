package vectors

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateSpaceKey is the vector-space-key grammar and length matrix
// (migration 0001, vector_spaces_key_format: ^[a-z0-9][a-z0-9._-]{0,127}$).
func TestValidateSpaceKey(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty", "", true},
		{"seeded space key", "qwen3-embedding-0.6b-1024-cosine-v1", false},
		{"simple", "space-a", false},
		{"with digits", "model-2-384", false},
		{"with underscore", "model_v1", false},
		{"with dot", "model.v1", false},
		{"with dash", "model-1", false},
		{"leading digit", "1space", false},
		{"single letter", "a", false},
		{"single digit", "1", false},
		{"max length 128", "a" + strings.Repeat("b", 127), false},
		{"over length 128", "a" + strings.Repeat("b", 128), true},
		{"leading dash", "-alpha", true},
		{"leading underscore", "_alpha", true},
		{"leading dot", ".alpha", true},
		{"uppercase", "Model", true},
		{"uppercase inside", "model-A", true},
		{"space char", "model name", true},
		{"slash", "model/name", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSpaceKey(tc.key)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateSpaceKey(%q) error = %v, want error = %v", tc.key, err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidSpaceKey) {
				t.Fatalf("ValidateSpaceKey(%q) error = %v, want ErrInvalidSpaceKey", tc.key, err)
			}
		})
	}
}

// TestValidateSpaceKeyDoesNotEchoInput proves a failed key never appears in
// the error message (the key is caller-controlled request material; logging
// discipline forbids echoing arbitrary caller content).
func TestValidateSpaceKeyDoesNotEchoInput(t *testing.T) {
	secret := "UPPERCASE-should-not-appear-in-error"
	err := ValidateSpaceKey(secret)
	if err == nil {
		t.Fatal("ValidateSpaceKey accepted an invalid key")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error message echoes the input key: %q", err.Error())
	}
}
