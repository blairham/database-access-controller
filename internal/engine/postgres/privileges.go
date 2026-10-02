// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"fmt"
	"sort"
	"strings"
)

// Valid privileges, as accepted by the DatabaseAccess API.
const (
	PrivSelect     = "SELECT"
	PrivInsert     = "INSERT"
	PrivUpdate     = "UPDATE"
	PrivDelete     = "DELETE"
	PrivTruncate   = "TRUNCATE"
	PrivReferences = "REFERENCES"
	PrivTrigger    = "TRIGGER"
	PrivUsage      = "USAGE"
	PrivCreate     = "CREATE"
)

var allPrivileges = map[string]bool{
	PrivSelect: true, PrivInsert: true, PrivUpdate: true, PrivDelete: true,
	PrivTruncate: true, PrivReferences: true, PrivTrigger: true,
	PrivUsage: true, PrivCreate: true,
}

// schemaOnly are the privileges that apply to a schema rather than to the
// relations inside it.
var schemaOnly = map[string]bool{PrivUsage: true, PrivCreate: true}

// sequenceAccepted is the complete set PostgreSQL accepts on a sequence.
// Granting anything else produces "invalid privilege type INSERT for sequence"
// and aborts the transaction, so the caller's list is intersected with this
// rather than passed through.
var sequenceAccepted = map[string]bool{PrivSelect: true, PrivUpdate: true, PrivUsage: true}

// PrivilegeSplit is one schema's privilege list divided by the object class
// each privilege can legally be granted on.
type PrivilegeSplit struct {
	// Schema privileges. USAGE is always present: without it the role cannot
	// resolve a name in the schema, which makes every table grant useless.
	Schema []string

	// Table privileges, granted ON ALL TABLES and registered as the default for
	// future tables.
	Table []string

	// Sequence privileges, the intersection of the request with what a sequence
	// accepts.
	Sequence []string
}

// SplitPrivileges divides privs by object class.
func SplitPrivileges(privs []string) (PrivilegeSplit, error) {
	var s PrivilegeSplit
	seen := map[string]bool{}

	for _, p := range privs {
		up := strings.ToUpper(strings.TrimSpace(p))
		if !allPrivileges[up] {
			return s, fmt.Errorf("unknown privilege %q", p)
		}
		if seen[up] {
			continue
		}
		seen[up] = true

		if !schemaOnly[up] {
			s.Table = append(s.Table, up)
		}
		if sequenceAccepted[up] {
			s.Sequence = append(s.Sequence, up)
		}
	}

	// USAGE on the schema is implied by asking for anything in it.
	s.Schema = []string{PrivUsage}
	if seen[PrivCreate] {
		s.Schema = append(s.Schema, PrivCreate)
	}

	sort.Strings(s.Table)
	sort.Strings(s.Sequence)
	return s, nil
}

// Join renders a privilege list for a GRANT statement.
func Join(privs []string) string { return strings.Join(privs, ", ") }
