package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vector-service/internal/apierr"
)

func TestDecodeJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
		dst  any
		want *apierr.Error
	}{
		{
			name: "valid object",
			body: `{"a":1,"b":"x"}`,
			dst: new(struct {
				A int    `json:"a"`
				B string `json:"b"`
			}),
			want: nil,
		},
		{
			name: "valid array",
			body: `[1,2,3]`,
			dst:  new([]int),
			want: nil,
		},
		{
			name: "valid number",
			body: `42`,
			dst:  new(int),
			want: nil,
		},
		{
			name: "unknown field",
			body: `{"a":1,"zzz":2}`,
			dst: new(struct {
				A int `json:"a"`
			}),
			want: apierr.ErrUnknownField,
		},
		{
			name: "malformed json",
			body: `{"a":`,
			dst:  new(struct{}),
			want: apierr.ErrInvalidJSON,
		},
		{
			name: "trailing content",
			body: `{"a":1} x`,
			dst: new(struct {
				A int `json:"a"`
			}),
			want: apierr.ErrInvalidJSON,
		},
		{
			name: "trailing value",
			body: `{"a":1} {"b":2}`,
			dst: new(struct {
				A int `json:"a"`
			}),
			want: apierr.ErrInvalidJSON,
		},
		{
			name: "wrong type for field",
			body: `{"a":"not-a-number"}`,
			dst: new(struct {
				A int `json:"a"`
			}),
			want: apierr.ErrInvalidJSON,
		},
		{
			name: "wrong type at top level",
			body: `{"a":1}`,
			dst:  new(int),
			want: apierr.ErrInvalidJSON,
		},
		{
			name: "empty body",
			body: ``,
			dst:  new(struct{}),
			want: apierr.ErrInvalidJSON,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			got := DecodeJSON(r, tc.dst)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("DecodeJSON() = %v, want %v", got, tc.want)
			}
			if tc.want != nil && got.Code != tc.want.Code {
				t.Fatalf("DecodeJSON() code = %s, want %s", got.Code, tc.want.Code)
			}
		})
	}

	t.Run("body capped over the bound", func(t *testing.T) {
		// A JSON array that is well-formed up to just past the cap: the
		// first read reaches the 8-byte boundary before the document is
		// complete, so the overflow surfaces as *http.MaxBytesError (the
		// reader is mid-value, not at a value boundary).
		body := `["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]`
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		CapBody(r, 8)
		got := DecodeJSON(r, new([]string))
		if got == nil || got.Code != apierr.CodeBodyTooLarge {
			t.Fatalf("DecodeJSON() = %v, want body_too_large", got)
		}
	})

	t.Run("nil body", func(t *testing.T) {
		got := DecodeJSON(&http.Request{}, new(struct{}))
		if got == nil || got.Code != apierr.CodeInvalidJSON {
			t.Fatalf("DecodeJSON() = %v, want invalid_json", got)
		}
	})
}

func TestCheckContentType(t *testing.T) {
	tests := []struct {
		name string
		ct   string
		want *apierr.Error
	}{
		{name: "application/json", ct: "application/json", want: nil},
		{name: "charset allowed", ct: "application/json; charset=utf-8", want: nil},
		{name: "case-insensitive", ct: "Application/JSON", want: nil},
		{name: "text/plain", ct: "text/plain", want: apierr.ErrInvalidContentType},
		{name: "json text", ct: "text/json", want: apierr.ErrInvalidContentType},
		{name: "missing header", ct: "", want: apierr.ErrInvalidContentType},
		{name: "form data", ct: "multipart/form-data; boundary=x", want: apierr.ErrInvalidContentType},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.ct != "" {
				r.Header.Set("Content-Type", tc.ct)
			}
			got := CheckContentType(r)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("CheckContentType() = %v, want %v", got, tc.want)
			}
			if tc.want != nil && got.Code != tc.want.Code {
				t.Fatalf("CheckContentType() code = %s, want %s", got.Code, tc.want.Code)
			}
		})
	}
}
