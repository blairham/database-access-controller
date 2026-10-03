// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/blairham/database-controller/internal/rdsca"
)

// rootCAs supplies the CAs a verify-ca or verify-full connection trusts. A
// variable so tests can substitute a CA they control.
var rootCAs = rdsca.Pool

// TokenSource mints the password used for an IAM-authenticated connection.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// ConnConfig describes an admin connection to PostgreSQL.
type ConnConfig struct {
	Host     string
	Port     int32
	Database string
	User     string
	SSLMode  string

	// Tokens supplies the IAM auth token used as the password. RDS rejects a
	// plaintext connection for an IAM user, so SSLMode must stay at require or
	// stricter when this is set -- and since the token is a bearer credential,
	// anything short of verify-full hands it to whoever answers the connection.
	//
	// Exactly one of Tokens and Password is set.
	Tokens TokenSource

	// Password is a static password, for a PostgreSQL that does not offer IAM
	// authentication -- a self-managed server, or one in a development rig.
	Password string
}

// Conn is a pgx-backed Execer and Inspector.
type Conn struct {
	conn *pgx.Conn
}

var (
	_ Execer    = (*Conn)(nil)
	_ Inspector = (*Conn)(nil)
)

// Connect opens an admin connection using an IAM auth token as the password.
func Connect(ctx context.Context, cfg ConnConfig) (*Conn, error) {
	port := cfg.Port
	if port == 0 {
		port = 5432
	}
	sslMode := cfg.SSLMode
	if sslMode == "" {
		// verify-full, not require: require encrypts but accepts any
		// certificate, so anyone able to answer on the endpoint's address
		// receives the admin credential. RDS certificates verify against the
		// embedded RDS CAs (see rootCAs).
		sslMode = "verify-full"
	}

	password := cfg.Password
	switch {
	case cfg.Tokens != nil && password != "":
		return nil, errors.New("both an IAM token source and a static password were supplied; set exactly one")
	case cfg.Tokens != nil:
		var err error
		password, err = cfg.Tokens.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("minting auth token: %w", err)
		}
	case password == "":
		return nil, errors.New("no credentials: set either an IAM token source or a password")
	}

	pgCfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d database=%s user=%s sslmode=%s",
		cfg.Host, port, cfg.Database, cfg.User, sslMode))
	if err != nil {
		return nil, fmt.Errorf("parsing connection config: %w", err)
	}
	pgCfg.Password = password
	if sslMode == "verify-ca" || sslMode == "verify-full" {
		pool, caErr := rootCAs()
		if caErr != nil {
			return nil, fmt.Errorf("loading CA certificates: %w", caErr)
		}
		trust(pgCfg, pool)
	}

	conn, err := pgx.ConnectConfig(ctx, pgCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s:%d/%s as %s: %w", cfg.Host, port, cfg.Database, cfg.User, err)
	}
	return &Conn{conn: conn}, nil
}

// Exec runs a statement.
func (c *Conn) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := c.conn.Exec(ctx, sql, args...)
	return err
}

// Close releases the connection.
func (c *Conn) Close(ctx context.Context) error { return c.conn.Close(ctx) }

// RoleExists reports whether the role is present.
func (c *Conn) RoleExists(ctx context.Context, role string) (bool, error) {
	var exists bool
	if err := c.conn.QueryRow(ctx, QueryRoleExists, role).Scan(&exists); err != nil {
		return false, fmt.Errorf("querying pg_roles for %q: %w", role, err)
	}
	return exists, nil
}

// SchemaExists reports whether the schema is present.
func (c *Conn) SchemaExists(ctx context.Context, schema string) (bool, error) {
	var exists bool
	if err := c.conn.QueryRow(ctx, QuerySchemaExists, schema).Scan(&exists); err != nil {
		return false, fmt.Errorf("querying pg_namespace for %q: %w", schema, err)
	}
	return exists, nil
}

// ObjectOwners returns the roles that own relations in the schema, unioned with
// the schema owner.
func (c *Conn) ObjectOwners(ctx context.Context, schema string) ([]string, error) {
	rows, err := c.conn.Query(ctx, QueryObjectOwners, schema)
	if err != nil {
		return nil, fmt.Errorf("querying owners of %q: %w", schema, err)
	}
	defer rows.Close()

	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, fmt.Errorf("scanning owner of %q: %w", schema, err)
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

// RelationsNotOwnedBy returns the relations in the schema not owned by owner.
func (c *Conn) RelationsNotOwnedBy(ctx context.Context, schema, owner string) ([]Relation, error) {
	rows, err := c.conn.Query(ctx, QueryRelationsNotOwnedBy, schema, owner)
	if err != nil {
		return nil, fmt.Errorf("querying relations in %q: %w", schema, err)
	}
	defer rows.Close()

	var rels []Relation
	for rows.Next() {
		var r Relation
		if err := rows.Scan(&r.Name, &r.Kind, &r.Owner); err != nil {
			return nil, fmt.Errorf("scanning relation in %q: %w", schema, err)
		}
		rels = append(rels, r)
	}
	return rels, rows.Err()
}

// trust makes every TLS configuration pgx will try verify against pool. pgx
// reads RootCAs at handshake time in both verify modes -- verify-full through
// crypto/tls, verify-ca through its own VerifyPeerCertificate -- so setting it
// after ParseConfig is enough.
func trust(cfg *pgx.ConnConfig, pool *x509.CertPool) {
	if cfg.TLSConfig != nil {
		cfg.TLSConfig.RootCAs = pool
	}
	for _, fb := range cfg.Fallbacks {
		if fb.TLSConfig != nil {
			fb.TLSConfig.RootCAs = pool
		}
	}
}
