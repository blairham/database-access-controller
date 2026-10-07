// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"fmt"
	"regexp"
	"strings"
)

// The rule behind both patterns: a name the controller writes must, if someone
// later types it without quotes, either mean the same object or fail to parse
// -- never silently name a different one. So uppercase stays out (unquoted
// Name folds to name), as do dots (a.b is schema-qualified) and "--" (it
// starts a comment, so an unquoted a--b reads as the role a).

// identPattern is the shape of a name that also reads the same unquoted. It
// applies to schemas, where hand-written SQL such as search_path is most often
// left unquoted.
var identPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// hyphenatedPattern also allows single hyphens between other characters, for
// roles and databases named after Kubernetes services (#27). Unquoted, such a
// name is a syntax error rather than a different object.
var hyphenatedPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*(-[a-z0-9_]+)*$`)

// ValidateIdent rejects an identifier that is not a plain lowercase name.
func ValidateIdent(kind, name string) error {
	return validate(kind, name, identPattern)
}

// ValidateHyphenatedIdent is ValidateIdent that also admits single hyphens
// between other characters. It is for role and database names only.
func ValidateHyphenatedIdent(kind, name string) error {
	return validate(kind, name, hyphenatedPattern)
}

// ValidateCatalogIdent checks only what PostgreSQL itself would get wrong: an
// empty name, or one it would truncate. It is for names read back from the
// catalog, such as relations under ownerOf (#30). Those already exist, so the
// naming rule has nothing to protect, and Entity Framework's Orders or
// __EFMigrationsHistory are legitimate; QuoteIdent alone keeps them one
// identifier.
func ValidateCatalogIdent(kind, name string) error {
	return validate(kind, name, nil)
}

// validate checks name's length and, unless pattern is nil, its shape.
func validate(kind, name string, pattern *regexp.Regexp) error {
	if name == "" {
		return fmt.Errorf("%s name is empty", kind)
	}
	if len(name) > 63 {
		// PostgreSQL silently truncates at NAMEDATALEN-1.
		return fmt.Errorf("%s name %q exceeds 63 bytes and would be silently truncated by PostgreSQL", kind, name)
	}
	if pattern != nil && !pattern.MatchString(name) {
		return fmt.Errorf("%s name %q must match %s", kind, name, pattern)
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
