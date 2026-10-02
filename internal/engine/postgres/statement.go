// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"strings"
)

// Execer runs a statement that returns no rows.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) error
}

// Statement is one SQL statement and the policy for handling its failure. It
// implements plan.Step.
type Statement struct {
	// SQL is the statement text. Identifiers are quoted inline because
	// PostgreSQL does not accept a parameter where an identifier is required;
	// everything else is parameterized.
	SQL string

	// Args are the positional parameters for SQL.
	Args []any

	// Why explains the statement. Printed by a dry run and by nothing else.
	Why string

	// BestEffort downgrades a failure to a warning.
	BestEffort bool

	// Ignore lists error substrings that are expected for this statement and
	// mean the work was already done.
	Ignore []string

	exec Execer
}

// Describe returns the SQL text.
func (s *Statement) Describe() string {
	sql := strings.TrimSpace(s.SQL)
	if !strings.HasSuffix(sql, ";") {
		sql += ";"
	}
	return sql
}

// Rationale returns the explanation for the statement.
func (s *Statement) Rationale() string { return s.Why }

// IsBestEffort reports whether a failure is a warning rather than fatal.
func (s *Statement) IsBestEffort() bool { return s.BestEffort }

// Tolerates reports whether err is one this statement expects.
//
// The script this replaces piped psql's stderr through `grep -v "already
// exists"`, which suppressed the exit status for every error, not just that
// one. Matching an explicit list keeps the narrow tolerance and drops the
// blanket one.
func (s *Statement) Tolerates(err error) bool {
	if err == nil {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, sub := range s.Ignore {
		if strings.Contains(msg, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

// Apply runs the statement.
func (s *Statement) Apply(ctx context.Context) error {
	if s.exec == nil {
		// A plan built for inspection only. Describe() is the whole product.
		return nil
	}
	return s.exec.Exec(ctx, s.SQL, s.Args...)
}
