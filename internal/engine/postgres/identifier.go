// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"fmt"
	"regexp"
	"strings"
)

// identPattern is the identifier shape this package accepts, deliberately
// narrower than PostgreSQL's own rules.
var identPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ValidateIdent rejects an identifier that is not a plain lowercase name.
func ValidateIdent(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s name is empty", kind)
	}
	if len(name) > 63 {
		// PostgreSQL silently truncates at NAMEDATALEN-1.
		return fmt.Errorf("%s name %q exceeds 63 bytes and would be silently truncated by PostgreSQL", kind, name)
	}
	if !identPattern.MatchString(name) {
		return fmt.Errorf("%s name %q must match %s", kind, name, identPattern)
	}
	return nil
}

// QuoteIdent renders name as a quoted SQL identifier, doubling embedded quotes.
// Identifiers cannot be bind parameters, so every one goes through here.
func QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// QuoteIdents renders a list of identifiers, comma-separated.
func QuoteIdents(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = QuoteIdent(n)
	}
	return strings.Join(out, ", ")
}
