// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

// Package plan models provisioning as an ordered list of steps that can be
// printed before they are run: Describe gives a dry run, and Apply reports
// per-step outcomes.
package plan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Step is one unit of provisioning work.
type Step interface {
	// Describe returns what the step will run, such as its SQL text.
	Describe() string

	// Apply performs the step.
	Apply(ctx context.Context) error

	// Rationale explains why the step exists. Printed by a dry run.
	Rationale() string

	// IsBestEffort reports whether a failure should be recorded as a warning
	// rather than aborting the plan.
	IsBestEffort() bool

	// Tolerates reports whether err is an expected, benign failure for this
	// step -- "role already exists" on a create, for instance.
	Tolerates(err error) bool
}

// Plan is an ordered, re-runnable set of steps. Order matters: a role must
// exist before it is granted to.
type Plan struct {
	steps []Step
}

// Add appends steps to the plan.
func (p *Plan) Add(steps ...Step) {
	for _, s := range steps {
		if s == nil {
			continue
		}
		p.steps = append(p.steps, s)
	}
}

// Steps returns the plan's steps in order.
func (p *Plan) Steps() []Step { return p.steps }

// Len returns the number of steps.
func (p *Plan) Len() int { return len(p.steps) }

// Describe renders the whole plan as annotated text, suitable for review.
func (p *Plan) Describe() string {
	var b strings.Builder
	for i, s := range p.steps {
		if i > 0 {
			b.WriteString("\n")
		}
		if r := s.Rationale(); r != "" {
			for _, line := range strings.Split(r, "\n") {
				b.WriteString("-- ")
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
		if s.IsBestEffort() {
			b.WriteString("-- best-effort: failure is recorded as a warning, not fatal\n")
		}
		b.WriteString(s.Describe())
		b.WriteString("\n")
	}
	return b.String()
}

// Hash fingerprints the plan's steps, for status.appliedPlanHash.
func (p *Plan) Hash() string {
	h := sha256.New()
	for _, s := range p.steps {
		fmt.Fprintf(h, "%s\x00", s.Describe())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Result reports what Apply did.
type Result struct {
	// Applied is the number of steps that ran without a fatal error.
	Applied int

	// Warnings holds one message per best-effort step that failed.
	Warnings []string

	// Tolerated is the number of steps whose expected error was ignored.
	Tolerated int
}

// Apply runs every step in order. A fatal error stops the plan and names the
// step; best-effort failures accumulate in Result.Warnings.
func (p *Plan) Apply(ctx context.Context) (Result, error) {
	var res Result
	for _, s := range p.steps {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		err := s.Apply(ctx)
		switch {
		case err == nil:
			res.Applied++
		case s.Tolerates(err):
			res.Tolerated++
			res.Applied++
		case s.IsBestEffort():
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("%s: %v", firstLine(s.Describe()), err))
		default:
			return res, fmt.Errorf("applying %q: %w", firstLine(s.Describe()), err)
		}
	}
	return res, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}
