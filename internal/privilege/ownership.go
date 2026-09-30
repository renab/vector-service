package privilege

import (
	"fmt"
	"sort"
)

// Subgraph is the runtime role's reachable membership subgraph over the
// complete pg_auth_members graph: the set of membership edges reachable by a
// breadth-first traversal starting from the role's OID. A step extends the
// search from a reached member to the roleid it is a member of.
type Subgraph struct {
	// Root is the OID the traversal started from.
	Root int32
	// Edges are the reachable membership edges, in deterministic order.
	Edges []MembershipEdge
	// Closure maps every reachable roleid to true, at any depth. It does not
	// include Root itself.
	Closure map[int32]bool
	// InheritClosure maps the roleids reachable exclusively through
	// inherit_option-true edges to true. Through these edges the roleid's
	// object privileges are inherited into the member automatically, and they
	// are the entries the effective-privilege union counts.
	InheritClosure map[int32]bool
}

// MembershipSubgraph computes the reachable membership subgraph from the full
// pg_auth_members edge list.
func MembershipSubgraph(root int32, edges []MembershipEdge) *Subgraph {
	byMember := make(map[int32][]MembershipEdge)
	for _, e := range edges {
		byMember[e.Member] = append(byMember[e.Member], e)
	}

	sg := &Subgraph{
		Root:           root,
		Closure:        map[int32]bool{},
		InheritClosure: map[int32]bool{},
	}

	// Deterministic BFS: frontier in OID order, edges sorted by roleid.
	frontier := []int32{root}
	for len(frontier) > 0 {
		member := frontier[0]
		frontier = frontier[1:]
		memberEdges := byMember[member]
		sort.Slice(memberEdges, func(i, j int) bool { return memberEdges[i].RoleID < memberEdges[j].RoleID })
		for _, e := range memberEdges {
			sg.Edges = append(sg.Edges, e)
			if !sg.Closure[e.RoleID] {
				sg.Closure[e.RoleID] = true
				frontier = append(frontier, e.RoleID)
			}
			if e.Inherit {
				sg.InheritClosure[e.RoleID] = true
			}
		}
	}
	return sg
}

// SetOptionEdge returns the first reachable edge with set_option true, if
// any. Such an edge lets the member SET ROLE into the granted role: an
// assumption right no privilege equality can bound.
func (s *Subgraph) SetOptionEdge() (MembershipEdge, bool) {
	for _, e := range s.Edges {
		if e.Set {
			return e, true
		}
	}
	return MembershipEdge{}, false
}

// AdminOptionEdge returns the first reachable edge with admin_option true,
// if any. Such an edge lets the member grant, revoke, or alter the
// membership: an unauthorized regrant channel.
func (s *Subgraph) AdminOptionEdge() (MembershipEdge, bool) {
	for _, e := range s.Edges {
		if e.Admin {
			return e, true
		}
	}
	return MembershipEdge{}, false
}

// OwnerMismatch is one in-scope object whose ownership violates the rule
// under check.
type OwnerMismatch struct {
	// Catalog is the catalog the object was found in: pg_database,
	// pg_namespace, pg_class, pg_type, or pg_proc.
	Catalog string
	// Object is the object's qualified name.
	Object string
	// Owner is the actual owner's role name.
	Owner string
}

// Error renders the mismatch for structured errors.
func (m OwnerMismatch) Error() string {
	return fmt.Sprintf("%s %s is owned by %s", m.Catalog, m.Object, m.Owner)
}

// OwnershipSweep is the swept catalog state of the connected database and
// the two migrated schemas.
type OwnershipSweep struct {
	Database  DatabaseInfo
	Schemas   []SchemaInfo
	Relations []RelationRow
	Types     []TypeRow
	Functions []FunctionRow
}

// CheckOwnership evaluates one ownership rule over the swept state.
// requireOwned is the canonical ownership gate (the migration identity must
// own the database, both schemas, and every in-scope pg_class, pg_type, and
// pg_proc row). !requireOwned is the runtime no-ownership rule (the runtime
// role must own none of them). Every violation is returned; an empty result
// means the rule holds.
func CheckOwnership(sweep OwnershipSweep, owner int32, roleName func(int32) string, requireOwned bool) []OwnerMismatch {
	var mismatches []OwnerMismatch
	owned := func(o int32) bool { return o == owner }
	add := func(catalog, object string, actual int32) {
		mismatches = append(mismatches, OwnerMismatch{
			Catalog: catalog,
			Object:  object,
			Owner:   roleName(actual),
		})
	}
	db := sweep.Database
	if requireOwned != owned(db.Owner) {
		add("pg_database", "database "+db.Name, db.Owner)
	}
	for _, s := range sweep.Schemas {
		if requireOwned != owned(s.Owner) {
			add("pg_namespace", "schema "+s.Name, s.Owner)
		}
	}
	for _, r := range sweep.Relations {
		if requireOwned != owned(r.Owner) {
			add("pg_class", r.Nsp+"."+r.Relname, r.Owner)
		}
	}
	for _, t := range sweep.Types {
		if requireOwned != owned(t.Owner) {
			add("pg_type", t.Nsp+"."+t.Typname, t.Owner)
		}
	}
	for _, f := range sweep.Functions {
		if requireOwned != owned(f.Owner) {
			add("pg_proc", f.Nsp+"."+f.Proname+"("+f.IdentityArgs+")", f.Owner)
		}
	}
	return mismatches
}
