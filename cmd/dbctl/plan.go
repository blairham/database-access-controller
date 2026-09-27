package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"

	dbv1alpha1 "github.com/blairham/database-controller/apis/db/v1alpha1"
	"github.com/blairham/database-controller/internal/controller/databaseaccess"
	"github.com/blairham/database-controller/internal/engine"
	"github.com/blairham/database-controller/internal/plan"
)

// loadAccess reads a DatabaseAccess manifest from a file or stdin.
func loadAccess(path string) (*dbv1alpha1.DatabaseAccess, error) {
	var (
		raw []byte
		err error
	)
	if path == "-" || path == "" {
		raw, err = os.ReadFile("/dev/stdin")
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var da dbv1alpha1.DatabaseAccess
	if err := yaml.UnmarshalStrict(raw, &da); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if da.Spec.Role == "" {
		return nil, fmt.Errorf("%s: spec.role is empty", path)
	}
	return &da, nil
}

// buildPlan connects to the database named in the manifest and plans against
// its actual current state.
func buildPlan(ctx context.Context, da *dbv1alpha1.DatabaseAccess) (*plan.Plan, engine.Engine, error) {
	eng, err := databaseaccess.DefaultEngineFactory(ctx, da.Spec)
	if err != nil {
		return nil, nil, err
	}
	p, err := eng.BuildPlan(ctx, databaseaccess.Access(da.Spec))
	if err != nil {
		eng.Close() //nolint:errcheck // already failing
		return nil, nil, err
	}
	return p, eng, nil
}

type planCommand struct{}

func (c *planCommand) Synopsis() string {
	return "Print the statements the controller would run, without running them"
}

func (c *planCommand) Help() string {
	return strings.TrimSpace(`
Usage: dbctl plan -f <manifest>

  Connects to the database named in a DatabaseAccess manifest, plans against
  its current state, and prints the statements the controller would run.

  Nothing is written. The plan reflects real state: which schemas exist, which
  roles own objects in them, and which relations would be reassigned.

Options:

  -f <path>   DatabaseAccess manifest, or - for stdin.
`)
}

func (c *planCommand) Run(args []string) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	path := fs.String("f", "-", "DatabaseAccess manifest, or - for stdin")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	ctx := context.Background()
	da, err := loadAccess(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	p, eng, err := buildPlan(ctx, da)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	defer eng.Close() //nolint:errcheck // read-only path

	fmt.Print(p.Describe())
	fmt.Fprintf(os.Stderr, "\n%d statement(s), plan %s\n", p.Len(), p.Hash())
	return 0
}

type applyCommand struct{}

func (c *applyCommand) Synopsis() string {
	return "Apply a DatabaseAccess manifest directly, outside the controller"
}

func (c *applyCommand) Help() string {
	return strings.TrimSpace(`
Usage: dbctl apply -f <manifest> [-yes]

  Applies the same plan the controller would apply. Intended for a database the
  controller does not yet manage, or for recovering one by hand.

  Prints the plan and asks for confirmation unless -yes is given.

Options:

  -f <path>   DatabaseAccess manifest, or - for stdin.
  -yes        Skip the confirmation prompt.
`)
}

func (c *applyCommand) Run(args []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	path := fs.String("f", "-", "DatabaseAccess manifest, or - for stdin")
	assumeYes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	ctx := context.Background()
	da, err := loadAccess(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	p, eng, err := buildPlan(ctx, da)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	defer eng.Close() //nolint:errcheck // best-effort teardown

	fmt.Print(p.Describe())
	fmt.Fprintf(os.Stderr, "\n%d statement(s) against %s/%s as %s\n",
		p.Len(), da.Spec.Instance.Endpoint, da.Spec.Instance.Database, da.Spec.Instance.AdminUser)

	if !*assumeYes {
		fmt.Fprint(os.Stderr, "apply? [y/N] ")
		var answer string
		fmt.Scanln(&answer) //nolint:errcheck // empty answer means no
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Fprintln(os.Stderr, "aborted")
			return 1
		}
	}

	res, err := p.Apply(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apply failed after %d statement(s): %v\n", res.Applied, err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "applied %d statement(s)\n", res.Applied)
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	return 0
}
