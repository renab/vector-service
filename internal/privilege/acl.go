package privilege

import (
	"fmt"
	"strconv"
	"strings"
)

// ACLEntry is one decoded entry of a PostgreSQL ACL (an aclitem).
type ACLEntry struct {
	// Grantee is the role name of the entry's grantee, or "" for PUBLIC.
	Grantee string
	// GranteeOID is the numeric grantee as it appears in the ACL text when
	// PostgreSQL recorded an OID instead of a name. Zero when the grantee is
	// PUBLIC or a name.
	GranteeOID int32
	// Public is true for the PUBLIC entry (empty grantee).
	Public bool
	// Privileges are the entry's privileges, decoded for the entry's kind.
	Privileges Set
	// WithGrantOption is true when the entry carries the grant option (the
	// "+" marker in the ACL text): the grantee may re-grant the entry's
	// privileges.
	WithGrantOption bool
}

// Grantor is the name of the role that granted the entry. It is informational
// only: the canonical boundary checks never depend on it.
type Grantor string

// aclLetterTable maps one privilege letter to its privilege name for a kind.
// It is the PostgreSQL 18 text-format alphabet (ACL_ALL_RIGHTS_STR in
// src/include/utils/acl.h: "arwdDxtXUCTcsAm"), restricted to the letters
// each object class may carry. PostgreSQL 18 standardized CREATE as 'C'
// and CONNECT as 'c'; TRIGGER is 't' and sequences carry USAGE, SELECT,
// and UPDATE as 'U', 'r', 'w'.
func aclLetterTable(k Kind) map[byte]string {
	switch k {
	case KindClass:
		return map[byte]string{
			'a': PrivInsert,
			'r': PrivSelect,
			'w': PrivUpdate,
			'd': PrivDelete,
			'D': PrivTruncate,
			'x': PrivReferences,
			't': PrivTrigger,
			'm': PrivMaintain,
		}
	case KindSequence:
		return map[byte]string{
			'U': PrivUsage,
			'r': PrivSelect,
			'w': PrivUpdate,
		}
	case KindDatabase:
		return map[byte]string{
			'C': PrivCreate,
			'T': PrivTemporary,
			'c': PrivConnect,
		}
	case KindNamespace:
		return map[byte]string{
			'C': PrivCreate,
			'U': PrivUsage,
		}
	case KindFunction:
		return map[byte]string{
			'X': PrivExecute,
		}
	case KindType:
		return map[byte]string{
			'U': PrivUsage,
		}
	case KindLanguage:
		return map[byte]string{
			'U': PrivUsage,
		}
	default:
		panic(fmt.Sprintf("privilege: unknown kind %d", int(k)))
	}
}

// splitACLEntries decodes the entry list of an ACL catalog column's text.
// The catalog ACL columns are aclitem[]: their text form is the array's text
// form, a comma-separated element list inside braces, where an element that
// contains a comma, a brace, a double quote, a backslash, a leading or
// trailing space, or is empty is single-quoted with backslash escapes. An
// unbraced input is the bare logical entry list of the documented format.
func splitACLEntries(acl string) ([]string, error) {
	s := strings.TrimSpace(acl)
	if s == "" {
		return nil, nil
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return strings.Split(s, ","), nil
	}
	body := s[1 : len(s)-1]
	if body == "" { // the array text form of the empty ACL
		return nil, nil
	}
	var entries []string
	var cur strings.Builder
	quoted := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case quoted && c == '\\':
			i++
			if i >= len(body) {
				return nil, fmt.Errorf("decode ACL %q: dangling escape in quoted entry", acl)
			}
			cur.WriteByte(body[i])
		case c == '\'':
			quoted = !quoted
		case c == ',' && !quoted:
			entries = append(entries, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if quoted {
		return nil, fmt.Errorf("decode ACL %q: unterminated quoted entry", acl)
	}
	entries = append(entries, cur.String())
	return entries, nil
}

// ParseACL decodes the PostgreSQL ACL text format for object kind k. The
// input is the exact text of an ACL catalog column (pg_database.datacl,
// pg_namespace.nspacl, pg_class.relacl, pg_attribute.attacl, pg_proc.proacl,
// or pg_type.typacl) — the aclitem[] array's text form, or the equivalent
// bare entry list. An empty or NULL ACL decodes to no entries.
//
// Each entry is of the form "grantee=privileges/grantor" with: an empty
// grantee for PUBLIC; privilege letters for the entry's kind (the letter
// "A" for all of the kind's privileges); an optional "+" marker for the
// grant option; and an optional "/grantor" suffix naming the grantor.
// Entries may appear in any state (no privileges, no grantor, no grantee).
//
// Decoding is fail-closed: a letter that is not defined for the kind, a
// malformed entry, or a malformed array container, is an error.
func ParseACL(acl string, k Kind) ([]ACLEntry, error) {
	if strings.TrimSpace(acl) == "" {
		return nil, nil
	}
	letters := aclLetterTable(k)
	kindAll := kindPrivileges(k)

	raws, err := splitACLEntries(acl)
	if err != nil {
		return nil, err
	}
	var entries []ACLEntry
	for _, raw := range raws {
		entry := ACLEntry{Privileges: Set{}}

		rest := raw
		// Grantor: everything after the last '/'. Role names may legally
		// contain '/' only when quoted at creation, in which case the text
		// form carries the name as-is; the grantor is never parsed further,
		// so the split is informational.
		if idx := strings.LastIndex(rest, "/"); idx >= 0 {
			rest = rest[:idx]
		}
		// Grantee: everything before the first '='.
		if idx := strings.Index(rest, "="); idx >= 0 {
			entry.Grantee = rest[:idx]
			rest = rest[idx+1:]
			if entry.Grantee == "" {
				entry.Public = true
			} else if oid, err := strconv.ParseInt(entry.Grantee, 10, 32); err == nil {
				entry.GranteeOID = int32(oid)
			}
		}
		// Privileges and the grant option.
		for _, c := range rest {
			switch {
			case c == '*':
				entry.WithGrantOption = true
			case c == 'A':
				for p := range kindAll {
					entry.Privileges[p] = struct{}{}
				}
			default:
				name, ok := letters[byte(c)]
				if !ok {
					return nil, fmt.Errorf(
						"decode ACL %q: privilege letter %q is not defined for this object class",
						acl, string(c),
					)
				}
				entry.Privileges[name] = struct{}{}
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// EntriesFor returns the entries of a decoded ACL whose grantee matches
// either the given role name or OID, or — when public — the PUBLIC entry.
func EntriesFor(entries []ACLEntry, name string, oid int32) []ACLEntry {
	var out []ACLEntry
	for _, e := range entries {
		if e.Public {
			if name == "" && oid == 0 {
				out = append(out, e)
			}
			continue
		}
		if name != "" && e.Grantee == name && e.GranteeOID == 0 {
			out = append(out, e)
		}
		if oid != 0 && e.GranteeOID == oid {
			out = append(out, e)
		}
	}
	return out
}

// PublicEntries returns the PUBLIC entries of a decoded ACL (there is at
// most one).
func PublicEntries(entries []ACLEntry) []ACLEntry {
	return EntriesFor(entries, "", 0)
}

// PrivilegesOf returns the union of the privileges carried by the entries.
func PrivilegesOf(entries []ACLEntry) Set {
	out := Set{}
	for _, e := range entries {
		for p := range e.Privileges {
			out[p] = struct{}{}
		}
	}
	return out
}

// AnyGrantOption reports whether any of the entries carries a grant option.
func AnyGrantOption(entries []ACLEntry) bool {
	for _, e := range entries {
		if e.WithGrantOption {
			return true
		}
	}
	return false
}
