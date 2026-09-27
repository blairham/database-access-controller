// Package plan models provisioning as an ordered list of steps that can be
// printed before they are run.
//
// Both controllers in this repo replace a Kubernetes Job that ran a script: the
// RDS one ran psql, the Kafka one ran a topic tool. Neither could be inspected
// without running it, and neither reported which half of the script succeeded.
// A Plan fixes both: Describe() gives a dry run, and Apply() reports per-step
// outcomes instead of a single exit code.
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
	// Describe returns the human-readable form of the step: the SQL text, the
	// admin API call, whatever the step will actually do.
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

// Plan is an ordered, re-runnable set of steps. Order is load-bearing: a role
// must exist before it is granted to, a topic before its config is altered.
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

// Hash fingerprints the plan's steps. A changed hash means the desired state
// changed and the plan is worth re-applying.
//
// This replaces hashing a script into a Job name. That workaround exists
// because a Job is immutable on .spec.template and a GitOps engine's
// server-side diff swallows the immutable-field rejection, so a changed script
// can go stale indefinitely while the resource reports as synced. A controller
// has no such constraint: the hash is an observation recorded in status, not
// the mechanism that forces the work to happen.
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

// Apply runs every step in order.
//
// A fatal error stops the plan and is returned with the step's description, so
// the caller knows exactly where provisioning stopped. Best-effort failures
// accumulate in Result.Warnings and never stop the plan: ALTER DEFAULT
// PRIVILEGES FOR ROLE x requires the admin role to hold x's privileges, and it
// does not hold all of them. Aborting there would take provisioning down for
// every service the moment one unreachable owner appeared.
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
