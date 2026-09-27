package postgres

import "context"

// Relation kinds as PostgreSQL records them in pg_class.relkind.
const (
	RelKindTable     = "r"
	RelKindPartition = "p"
	RelKindView      = "v"
	RelKindMatView   = "m"
	RelKindSequence  = "S"
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
}

// Queries used by the pgx-backed Inspector. They are exported so the CLI can
// run the same reads without linking the controller.
const (
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
	QueryRelationsNotOwnedBy = `
SELECT c.relname, c.relkind::text, pg_get_userbyid(c.relowner)
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = $1
   AND c.relkind IN ('r', 'p', 'v', 'm', 'S')
   AND pg_get_userbyid(c.relowner) <> $2
 ORDER BY c.relkind, c.relname`
)
