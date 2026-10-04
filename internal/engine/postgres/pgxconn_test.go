// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

//go:build integration || equivalence

package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// pgxConn adapts a pgx connection to Execer and Inspector for tests that talk
// to a real server. It is shared by the integration and equivalence suites,
// which is why it carries both build tags.
type pgxConn struct{ c *pgx.Conn }

func (p pgxConn) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := p.c.Exec(ctx, sql, args...)
	return err
}

func (p pgxConn) RoleExists(ctx context.Context, role string) (bool, error) {
	var b bool
	err := p.c.QueryRow(ctx, QueryRoleExists, role).Scan(&b)
	return b, err
}

func (p pgxConn) SchemaExists(ctx context.Context, schema string) (bool, error) {
	var b bool
	err := p.c.QueryRow(ctx, QuerySchemaExists, schema).Scan(&b)
	return b, err
}

func (p pgxConn) ObjectOwners(ctx context.Context, schema string) ([]string, error) {
	rows, err := p.c.Query(ctx, QueryObjectOwners, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p pgxConn) RelationsNotOwnedBy(ctx context.Context, schema, owner string) ([]Relation, error) {
	rows, err := p.c.Query(ctx, QueryRelationsNotOwnedBy, schema, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Relation
	for rows.Next() {
		var r Relation
		if err := rows.Scan(&r.Name, &r.Kind, &r.Owner); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// The ACL reads delegate to the production Conn, so the integration and
// equivalence suites exercise the exact queries the controller runs rather
// than a second copy of them.

func (p pgxConn) prod() *Conn { return &Conn{conn: p.c} }

func (p pgxConn) RoleIsMemberOf(ctx context.Context, member, group string) (bool, error) {
	return p.prod().RoleIsMemberOf(ctx, member, group)
}

func (p pgxConn) DatabasePrivileges(ctx context.Context, database, grantee string) ([]string, error) {
	return p.prod().DatabasePrivileges(ctx, database, grantee)
}

func (p pgxConn) SchemaPrivileges(ctx context.Context, schema, grantee string) ([]string, error) {
	return p.prod().SchemaPrivileges(ctx, schema, grantee)
}

func (p pgxConn) RelationsLackPrivileges(
	ctx context.Context,
	schema, grantee string,
	kinds, privs []string,
) (bool, error) {
	return p.prod().RelationsLackPrivileges(ctx, schema, grantee, kinds, privs)
}

func (p pgxConn) DefaultPrivileges(ctx context.Context, owner, schema, objType, grantee string) ([]string, error) {
	return p.prod().DefaultPrivileges(ctx, owner, schema, objType, grantee)
}

func (p pgxConn) AdminAccessTo(ctx context.Context, role string) (AdminAccess, error) {
	return p.prod().AdminAccessTo(ctx, role)
}
