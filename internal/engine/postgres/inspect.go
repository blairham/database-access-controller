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
//
// ALTER TABLE is permissive enough for a view in PostgreSQL, but the explicit
// verb is what a reader expects to find when grepping for how a view got
// reassigned.
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
//
// Planning reads before it writes so the plan holds concrete statements. The
// job this replaces shipped server-side DO blocks that looped over pg_class at
// execution time, which meant nothing could be reviewed before it ran and the
// log reported only that the block completed.
type Inspector interface {
	// RoleExists reports whether the role is present in pg_roles.
	RoleExists(ctx context.Context, role string) (bool, error)

	// SchemaExists reports whether the schema is present in pg_namespace.
	SchemaExists(ctx context.Context, schema string) (bool, error)

	// ObjectOwners returns the distinct roles that own relations in the schema,
	// unioned with the schema's own owner.
	//
	// Owners come from pg_class, not from pg_namespace.nspowner alone. The
	// schema owner is not reliably the object owner: a schema created by an
	// admin and populated by an application role has two different answers, and
	// deriving from nspowner alone silently leaves every relation in it without
	// a default privilege registered. The schema owner is
	// unioned in anyway so a greenfield schema with no relations yet still gets
	// a default registered for whoever creates the first one.
	ObjectOwners(ctx context.Context, schema string) ([]string, error)

	// RelationsNotOwnedBy returns the relations in the schema whose owner is
	// not the given role, across tables, partitions, sequences, views and
	// materialized views.
	RelationsNotOwnedBy(ctx context.Context, schema, owner string) ([]Relation, error)

	// The reads below make the plan a diff: a GRANT is planned only when the
	// privilege it confers is not already there. Without them every reconcile
	// re-issued every grant, so a plan could never come back empty and
	// Observe mode would report the same statements forever on a database
	// that needed nothing.
	//
	// ALL of them read EXPLICIT ACL entries (aclexplode over the stored ACL),
	// never has_*_privilege. has_*_privilege answers "can this role do it",
	// counting privileges inherited through role membership and the implicit
	// rights of an owner, so it would call a grant present that was never
	// made -- and the next migration that replaces the object, or the next
	// change to that membership, would silently take the access away. The
	// provisioner this replaces wrote explicit grants; so does this one.
	//
	// A NULL ACL means "the built-in default", which carries no explicit entry
	// for the role, so it reads as missing and the GRANT is planned. That is
	// also what makes the result match the old Job's: a GRANT on an object
	// with a NULL ACL materializes it.

	// RoleIsMemberOf reports whether member holds a direct membership in group,
	// granted by anyone. On PostgreSQL 16 one membership can be recorded once
	// per grantor; any of them is enough to authenticate, so one is enough to
	// skip the GRANT.
	RoleIsMemberOf(ctx context.Context, member, group string) (bool, error)

	// DatabasePrivileges returns the privileges explicitly granted to grantee
	// on the database.
	DatabasePrivileges(ctx context.Context, database, grantee string) ([]string, error)

	// SchemaPrivileges returns the privileges explicitly granted to grantee on
	// the schema.
	SchemaPrivileges(ctx context.Context, schema, grantee string) ([]string, error)

	// RelationsLackPrivileges reports whether any relation of the given kinds
	// in the schema is missing an explicit grant to grantee of any of privs.
	// One missing privilege on one relation is enough to plan the
	// schema-wide GRANT; re-granting what is already there is harmless.
	RelationsLackPrivileges(ctx context.Context, schema, grantee string, kinds, privs []string) (bool, error)

	// DefaultPrivileges returns the privileges registered by ALTER DEFAULT
	// PRIVILEGES FOR ROLE owner IN SCHEMA schema for objType ("r" for tables,
	// "S" for sequences) to grantee.
	DefaultPrivileges(ctx context.Context, owner, schema, objType, grantee string) ([]string, error)

	// AdminAccessTo reports what the CONNECTING role (current_user) can do
	// with role. Unlike the ACL reads above this one is deliberately
	// EFFECTIVE (pg_has_role): the question is what the admin can do right
	// now, through any path, not which grants are recorded.
	AdminAccessTo(ctx context.Context, role string) (AdminAccess, error)
}

// AdminAccess is the connecting role's standing with another role.
//
// ownerOf needs both halves. SET is what PostgreSQL 16 checks for
// `CREATE SCHEMA ... AUTHORIZATION role` and `ALTER ... OWNER TO role`.
// INHERIT (pg_has_role USAGE) is what lets the admin act with the role's own
// privileges, which granting on a schema the role owns requires. With SET
// alone that GRANT fails with "permission denied for schema" -- measured on
// PostgreSQL 16.
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

	// QueryObjectOwners unions relation owners with the schema owner. The
	// relkind filter covers ordinary and partitioned tables, views,
	// materialized views and sequences -- the same set whose ownership is
	// reassigned, so the two halves agree.
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

	// QueryRelationsNotOwnedBy drives off pg_class rather than pg_tables and
	// pg_sequences so one query covers every relkind. Driving off those two
	// views is how views came to be missed: pg_tables is relkind IN ('r','p')
	// and pg_sequences is 'S', so a view was never reassigned no matter how
	// long ownership was requested. That is not cosmetic -- replacing a view
	// requires ownership of it, so a migration doing DROP VIEW / CREATE OR
	// REPLACE VIEW fails with "must be owner of view" while every table it
	// reads was correctly reassigned.
	//
	// SEQUENCES OWNED BY A COLUMN ARE EXCLUDED. A serial or identity sequence
	// is auto-dependent on its table's column (pg_depend.deptype = 'a'), and
	// PostgreSQL refuses to change its owner independently:
	//
	//	ERROR: cannot change owner of sequence "events_id_seq" (SQLSTATE 0A000)
	//	DETAIL: Sequence "events_id_seq" is linked to table "events".
	//
	// Its owner follows the table's, so reassigning the table is both
	// necessary and sufficient. Emitting the ALTER at all is an error, not
	// merely redundant.
	//
	// ORDER IS LOAD-BEARING for the same reason: tables come first, so a
	// standalone sequence that a table's reassignment would have fixed is
	// never attempted ahead of it.
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
	// The ACL reads. grantee is resolved by name inside the query: a role that
	// does not exist yet matches no entry, so everything reads as missing,
	// which is right -- the plan is about to create it.

	// QueryAdminAccessTo returns current_user's effective standing with $1.
	// A superuser reads true everywhere, which is why a suite connecting as
	// postgres can never see the failures this guards against.
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

	// QueryRelationsLackPrivileges pairs every relation of the requested kinds
	// with every wanted privilege and looks for one pair with no explicit
	// entry. aclexplode of a NULL ACL yields no rows, so a relation still on
	// the default ACL lacks everything.
	//
	// Relations the grantee OWNS are skipped. An owner holds every privilege
	// implicitly, and PostgreSQL does not record an owner's self-grant on an
	// object created after its own default privileges: such a table keeps a
	// NULL relacl. Counting it would plan the GRANT on every reconcile forever
	// and never report converged -- what stg showed for trader_tools and
	// trading_reports_ro. If ownership later moves away, the relation is no
	// longer skipped and the GRANT comes back.
	//
	// IS DISTINCT FROM, not <>: a grantee that does not exist yet resolves to
	// NULL, and `relowner <> NULL` would drop EVERY relation and plan nothing.
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

// Relation kinds a schema-wide GRANT reaches, as PostgreSQL enumerates them
// for ON ALL TABLES / ON ALL SEQUENCES IN SCHEMA. ALL TABLES includes views,
// materialized views, foreign and partitioned tables -- so the diff must
// look at the same set, or a view missing its grant would read as converged.
var (
	tableGrantKinds    = []string{RelKindTable, RelKindPartition, RelKindView, RelKindMatView, RelKindForeign}
	sequenceGrantKinds = []string{RelKindSequence}
)

// Default-ACL object types, as pg_default_acl.defaclobjtype records them.
const (
	defaclTables    = "r"
	defaclSequences = "S"
)
