// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"reflect"
	"testing"
)

func TestSplitPrivileges(t *testing.T) {
	tests := []struct {
		name     string
		privs    []string
		schema   []string
		table    []string
		sequence []string
	}{
		{
			name:     "read only",
			privs:    []string{"SELECT"},
			schema:   []string{"USAGE"},
			table:    []string{"SELECT"},
			sequence: []string{"SELECT"},
		},
		{
			// The regression this guards: INSERT reaching a sequence grant
			// produces "invalid privilege type INSERT for sequence", which
			// aborts the transaction under ON_ERROR_STOP and takes the whole
			// provisioning run with it.
			name:     "write privileges never reach the sequence grant",
			privs:    []string{"SELECT", "INSERT", "UPDATE", "DELETE"},
			schema:   []string{"USAGE"},
			table:    []string{"DELETE", "INSERT", "SELECT", "UPDATE"},
			sequence: []string{"SELECT", "UPDATE"},
		},
		{
			name:     "CREATE is a schema privilege and not a table one",
			privs:    []string{"SELECT", "CREATE"},
			schema:   []string{"USAGE", "CREATE"},
			table:    []string{"SELECT"},
			sequence: []string{"SELECT"},
		},
		{
			name:     "USAGE reaches sequences but not tables",
			privs:    []string{"USAGE", "SELECT"},
			schema:   []string{"USAGE"},
			table:    []string{"SELECT"},
			sequence: []string{"SELECT", "USAGE"},
		},
		{
			name:     "duplicates collapse",
			privs:    []string{"SELECT", "SELECT", "select"},
			schema:   []string{"USAGE"},
			table:    []string{"SELECT"},
			sequence: []string{"SELECT"},
		},
		{
			name:     "table-only privileges leave the sequence grant empty",
			privs:    []string{"TRUNCATE", "TRIGGER"},
			schema:   []string{"USAGE"},
			table:    []string{"TRIGGER", "TRUNCATE"},
			sequence: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SplitPrivileges(tt.privs)
			if err != nil {
				t.Fatalf("SplitPrivileges(%v) returned error: %v", tt.privs, err)
			}
			if !reflect.DeepEqual(got.Schema, tt.schema) {
				t.Errorf("schema privileges = %v, want %v", got.Schema, tt.schema)
			}
			if !reflect.DeepEqual(got.Table, tt.table) {
				t.Errorf("table privileges = %v, want %v", got.Table, tt.table)
			}
			if !reflect.DeepEqual(got.Sequence, tt.sequence) {
				t.Errorf("sequence privileges = %v, want %v", got.Sequence, tt.sequence)
			}
		})
	}
}

func TestSplitPrivilegesRejectsUnknown(t *testing.T) {
	if _, err := SplitPrivileges([]string{"SELECT", "SUPERUSER"}); err == nil {
		t.Fatal("SplitPrivileges accepted SUPERUSER, want an error")
	}
}

func TestValidateIdent(t *testing.T) {
	valid := []string{"app", "app_writer", "_leading", "a1"}
	for _, name := range valid {
		if err := ValidateIdent("role", name); err != nil {
			t.Errorf("ValidateIdent(%q) = %v, want nil", name, err)
		}
	}

	invalid := map[string]string{
		"empty":           "",
		"uppercase":       "TradeWriter",
		"leading digit":   "1role",
		"embedded quote":  `ro"le`,
		"statement break": "role; DROP DATABASE appdb",
		"hyphen":          "trade-writer",
		"over 63 bytes":   "a123456789012345678901234567890123456789012345678901234567890123",
	}
	for why, name := range invalid {
		if err := ValidateIdent("role", name); err == nil {
			t.Errorf("ValidateIdent(%q) accepted a %s, want an error", name, why)
		}
	}
}

func TestValidateHyphenatedIdent(t *testing.T) {
	valid := []string{"app", "_leading", "trade-writer", "backend-service-xyz-dev", "a-b-c", "_x-1"}
	for _, name := range valid {
		if err := ValidateHyphenatedIdent("role", name); err != nil {
			t.Errorf("ValidateHyphenatedIdent(%q) = %v, want nil", name, err)
		}
	}

	invalid := map[string]string{
		"empty":           "",
		"uppercase":       "Trade-Writer",
		"leading digit":   "1-role",
		"embedded quote":  `ro"le-x`,
		"statement break": "role-x; DROP DATABASE appdb",
		"leading hyphen":  "-writer",
		"trailing hyphen": "writer-",
		"double hyphen":   "trade--writer",
		"dot":             "trade.writer",
		"over 63 bytes":   "a-23456789012345678901234567890123456789012345678901234567890123",
	}
	for why, name := range invalid {
		if err := ValidateHyphenatedIdent("role", name); err == nil {
			t.Errorf("ValidateHyphenatedIdent(%q) accepted a %s, want an error", name, why)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	if got, want := QuoteIdent("app"), `"app"`; got != want {
		t.Errorf("QuoteIdent = %q, want %q", got, want)
	}
	if got, want := QuoteIdent(`we"ird`), `"we""ird"`; got != want {
		t.Errorf("QuoteIdent did not double the embedded quote: %q, want %q", got, want)
	}
}
