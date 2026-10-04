// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package postgres

import "context"

// Relation kinds as PostgreSQL records them in pg_class.relkind.
const (
	RelKindTable     = "r"
	RelKindPartition = "p"
	RelKindView      = "v"
	RelKindMatView   = "m"
	RelKindSequence  = "S"
	RelKindForeign   = "f"
)

// Relation is one object in a schema, with the role that owns it.
type Relation struct {
	Name  string
	Kind  string
	Owner string
}

// AlterVerb returns the ALTER form that reassigns ownership of this relation.
func (r Relation) AlterVerb() string {
	switch r.Kind {
	case RelKindMatView:
		return "ALTER MATERIALIZED VIEW"
	case RelKindView:
		return "ALTER VIEW"
	case RelKindSequence:
		return "ALTER SEQUENCE"
	default:
		return "ALTER TABLE"
	}
}

// Inspector reads the current state of a PostgreSQL database.
type Inspector interface {
	// RoleExists reports whether the role is present in pg_roles.
	RoleExists(ctx context.Context, role string) (bool, error)

	// SchemaExists reports whether the schema is present in pg_namespace.
	SchemaExists(ctx context.Context, schema string) (bool, error)

	// ObjectOwners returns the distinct roles that own relations in the schema
	// (from pg_class; the schema owner is often not the object owner), unioned
	// with the schema's owner so an empty schema still gets a default.
	ObjectOwners(ctx context.Context, schema string) ([]string, error)

	// RelationsNotOwnedBy returns the relations in the schema whose owner is
	// not the given role, across tables, partitions, sequences, views and
	// materialized views.
	RelationsNotOwnedBy(ctx context.Context, schema, owner string) ([]Relation, error)

	// The reads below make the plan a diff. They read EXPLICIT ACL entries
	// (aclexplode), never has_*_privilege, which counts inherited and owner
	// rights and would call a grant present that was never made. A NULL ACL
	// has no explicit entries, so it reads as missing.

	// RoleIsMemberOf reports whether member holds a direct membership in group,
	// from any grantor.
	RoleIsMemberOf(ctx context.Context, member, group string) (bool, error)

	// DatabasePrivileges returns the privileges explicitly granted to grantee
	// on the database.
	DatabasePrivileges(ctx context.Context, database, grantee string) ([]string, error)

	// SchemaPrivileges returns the privileges explicitly granted to grantee on
	// the schema.
	SchemaPrivileges(ctx context.Context, schema, grantee string) ([]string, error)

	// RelationsLackPrivileges reports whether any relation of the given kinds
	// in the schema is missing an explicit grant to grantee of any of privs.
	RelationsLackPrivileges(ctx context.Context, schema, grantee string, kinds, privs []string) (bool, error)

	// DefaultPrivileges returns the privileges registered by ALTER DEFAULT
	// PRIVILEGES FOR ROLE owner IN SCHEMA schema for objType ("r" for tables,
	// "S" for sequences) to grantee.
	DefaultPrivileges(ctx context.Context, owner, schema, objType, grantee string) ([]string, error)

	// AdminAccessTo reports what current_user can do with role. Unlike the
	// ACL reads above it is effective (pg_has_role), not explicit.
	AdminAccessTo(ctx context.Context, role string) (AdminAccess, error)
}

// AdminAccess is the connecting role's standing with another role. ownerOf
// needs SET (for CREATE SCHEMA ... AUTHORIZATION and OWNER TO) and INHERIT (to
// grant on a schema the role owns).
type AdminAccess struct {
	// Admin is current_user, for error messages.
	Admin string
	// Set: the admin may SET ROLE to role.
	Set bool
	// Inherit: the admin holds role's privileges.
	Inherit bool
	// Grant: the admin holds ADMIN OPTION on role, so it can grant itself
	// the membership it lacks.
	Grant bool
}

// Queries used by the pgx-backed Inspector. They are exported so the CLI can
// run the same reads without linking the controller.
const (
	QueryRoleExists = `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`

	QuerySchemaExists = `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`

	// QueryObjectOwners uses the same relkinds whose ownership is reassigned.
	QueryObjectOwners = `
SELECT pg_get_userbyid(c.relowner) AS owner
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = $1
   AND c.relkind IN ('r', 'p', 'v', 'm', 'S')
UNION
SELECT pg_get_userbyid(n.nspowner)
  FROM pg_namespace n
 WHERE n.nspname = $1
 ORDER BY owner`

	// QueryRelationsNotOwnedBy reads pg_class so views are included (pg_tables
	// and pg_sequences miss them). Column-owned sequences (deptype 'a') are
	// excluded: PostgreSQL refuses to change their owner independently, and
	// they follow their table. Tables sort first.
	QueryRelationsNotOwnedBy = `
SELECT c.relname, c.relkind::text, pg_get_userbyid(c.relowner)
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = $1
   AND c.relkind IN ('r', 'p', 'v', 'm', 'S')
   AND pg_get_userbyid(c.relowner) <> $2
   AND NOT (
     c.relkind = 'S'
     AND EXISTS (
       SELECT 1
         FROM pg_depend d
        WHERE d.classid = 'pg_class'::regclass
          AND d.objid = c.oid
          AND d.refclassid = 'pg_class'::regclass
          AND d.deptype = 'a'
     )
   )
 ORDER BY CASE c.relkind
            WHEN 'r' THEN 0
            WHEN 'p' THEN 0
            WHEN 'S' THEN 1
            ELSE 2
          END,
          c.relname`

	// QueryAdminAccessTo returns current_user's effective standing with $1. A
	// superuser reads true everywhere.
	QueryAdminAccessTo = `
SELECT current_user::text,
       pg_has_role(current_user, r.oid, 'SET'),
       pg_has_role(current_user, r.oid, 'USAGE'),
       pg_has_role(current_user, r.oid, 'MEMBER WITH ADMIN OPTION')
  FROM pg_roles r
 WHERE r.rolname = $1`

	QueryRoleIsMemberOf = `
SELECT EXISTS (
  SELECT 1
    FROM pg_auth_members a
    JOIN pg_roles m ON m.oid = a.member
    JOIN pg_roles g ON g.oid = a.roleid
   WHERE m.rolname = $1
     AND g.rolname = $2
)`

	// The ACL reads resolve grantee by name, so a role that does not exist yet
	// reads as holding nothing.
	QueryDatabasePrivileges = `
SELECT DISTINCT a.privilege_type
  FROM pg_database d, aclexplode(d.datacl) a
 WHERE d.datname = $1
   AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $2)
 ORDER BY 1`

	QuerySchemaPrivileges = `
SELECT DISTINCT a.privilege_type
  FROM pg_namespace n, aclexplode(n.nspacl) a
 WHERE n.nspname = $1
   AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $2)
 ORDER BY 1`

	// QueryRelationsLackPrivileges looks for any (relation, privilege) pair
	// with no explicit ACL entry. Relations the grantee owns are skipped: an
	// owner's self-grant is never recorded, so they would never converge. IS
	// DISTINCT FROM because a grantee that does not exist yet resolves to NULL.
	QueryRelationsLackPrivileges = `
SELECT EXISTS (
  SELECT 1
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
   CROSS JOIN unnest($4::text[]) AS want(priv)
   WHERE n.nspname = $1
     AND c.relkind::text = ANY($3::text[])
     AND c.relowner IS DISTINCT FROM (SELECT oid FROM pg_roles WHERE rolname = $2)
     AND NOT EXISTS (
       SELECT 1
         FROM aclexplode(c.relacl) a
        WHERE a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $2)
          AND a.privilege_type = want.priv
     )
)`

	QueryDefaultPrivileges = `
SELECT DISTINCT a.privilege_type
  FROM pg_default_acl d
  JOIN pg_namespace n ON n.oid = d.defaclnamespace,
       aclexplode(d.defaclacl) a
 WHERE d.defaclrole = (SELECT oid FROM pg_roles WHERE rolname = $1)
   AND n.nspname = $2
   AND d.defaclobjtype::text = $3
   AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $4)
 ORDER BY 1`
)

// Relation kinds that ON ALL TABLES / ON ALL SEQUENCES IN SCHEMA reach. The
// diff must check the same set.
var (
	tableGrantKinds    = []string{RelKindTable, RelKindPartition, RelKindView, RelKindMatView, RelKindForeign}
	sequenceGrantKinds = []string{RelKindSequence}
)

// Default-ACL object types, as pg_default_acl.defaclobjtype records them.
const (
	defaclTables    = "r"
	defaclSequences = "S"
)
