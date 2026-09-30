package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vector-service/internal/dbctx"
)

func TestResolveRequestID(t *testing.T) {
	tests := []struct {
		name     string
		inbound  string
		wantEcho bool
	}{
		{name: "valid id echoed", inbound: "req-123", wantEcho: true},
		{name: "uuid inbound echoed", inbound: "0199f31e-2000-7000-8000-0000000000a1", wantEcho: true},
		{name: "max length accepted", inbound: strings.Repeat("a", 128), wantEcho: true},
		{name: "over 128 replaced", inbound: strings.Repeat("a", 129), wantEcho: false},
		{name: "space replaced", inbound: "ab cd", wantEcho: false},
		{name: "control char replaced", inbound: "ab\x01cd", wantEcho: false},
		{name: "del char replaced", inbound: "ab\x7fcd", wantEcho: false},
		{name: "non-ascii replaced", inbound: "abcé", wantEcho: false},
		{name: "missing header replaced", inbound: "", wantEcho: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.inbound != "" {
				r.Header.Set(RequestIDHeader, tc.inbound)
			}
			id, nctx := ResolveRequestID(context.Background(), r)
			if tc.wantEcho {
				if id != tc.inbound {
					t.Fatalf("ResolveRequestID() = %q, want echo %q", id, tc.inbound)
				}
			} else {
				if id == "" || id == tc.inbound {
					t.Fatalf("ResolveRequestID() = %q, want generated (not %q)", id, tc.inbound)
				}
				if len(id) != 32 {
					t.Fatalf("generated id = %q, want 32-char hex", id)
				}
			}
			if got := dbctx.RequestID(nctx); got != id {
				t.Fatalf("context request ID = %q, want %q", got, id)
			}
		})
	}
}

func TestValidRequestID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{id: "abc", want: true},
		{id: strings.Repeat("a", 128), want: true},
		{id: strings.Repeat("a", 129), want: false},
		{id: "", want: false},
		{id: "a b", want: false},
		{id: "a\tb", want: false},
		{id: "a\x01b", want: false},
		{id: "a\x7fb", want: false},
		{id: "aé", want: false},
		{id: "!~", want: true},
	}
	for _, tc := range tests {
		if got := validRequestID(tc.id); got != tc.want {
			t.Errorf("validRequestID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
