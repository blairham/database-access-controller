// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"strings"
	"testing"
)

// identifierSeeds are the shapes an attacker writing a DatabaseAccess would
// try first: statement breaks, embedded quotes, comments, whitespace, unicode
// look-alikes, and lengths either side of PostgreSQL's 63-byte limit.
var identifierSeeds = []string{
	"app", "app_writer", "_leading", "a1", "",
	"TradeWriter", "1role", `ro"le`, `"`, `""`, `"; DROP DATABASE appdb; --`,
	"role; DROP DATABASE appdb", "role -- comment", "role/*x*/", "trade-writer",
	"tab\there", "new\nline", "nul\x00byte", "ápp", "аpp", // second is Cyrillic а
	strings.Repeat("a", 63), strings.Repeat("a", 64),
}

// FuzzValidateIdent pins that an accepted identifier is always a plain
// lowercase name of at most 63 bytes.
func FuzzValidateIdent(f *testing.F) {
	for _, s := range identifierSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if ValidateIdent("role", name) != nil {
			return
		}
		if name == "" || len(name) > 63 {
			t.Fatalf("accepted %q of length %d", name, len(name))
		}
		if name[0] >= '0' && name[0] <= '9' {
			t.Fatalf("accepted %q, which starts with a digit", name)
		}
		for i := 0; i < len(name); i++ {
			c := name[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
				t.Fatalf("accepted %q, which contains byte %q at %d", name, c, i)
			}
		}
		if got, want := QuoteIdent(name), `"`+name+`"`; got != want {
			t.Fatalf("QuoteIdent(%q) = %q, want %q for a validated name", name, got, want)
		}
	})
}

// FuzzQuoteIdent pins that, whatever the input, the output is exactly one
// quoted identifier that reads back as the input.
func FuzzQuoteIdent(f *testing.F) {
	for _, s := range identifierSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		quoted := QuoteIdent(name)
		got, rest, ok := readQuotedIdent(quoted)
		if !ok {
			t.Fatalf("QuoteIdent(%q) = %q, which is not a well-formed quoted identifier", name, quoted)
		}
		if rest != "" {
			t.Fatalf("QuoteIdent(%q) = %q, which ends the identifier early and leaves %q", name, quoted, rest)
		}
		if got != name {
			t.Fatalf("QuoteIdent(%q) = %q, which reads back as %q", name, quoted, got)
		}
	})
}

// readQuotedIdent reads one PostgreSQL double-quoted identifier from the start
// of s, returning it, the remainder, and whether s began with a complete one.
// Written independently of QuoteIdent.
func readQuotedIdent(s string) (ident, rest string, ok bool) {
	if s == "" || s[0] != '"' {
		return "", "", false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			b.WriteByte('"')
			i++
			continue
		}
		return b.String(), s[i+1:], true
	}
	return "", "", false
}
