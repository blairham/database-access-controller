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
