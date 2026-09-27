// Command dbctl runs the controllers' planning logic from a terminal.
//
// It exists because the thing it replaces could not be inspected. Provisioning
// lived as Helm template text inside a Job manifest: the only way to find out
// what it would do to a database was to let it do it, and the only record
// afterwards was the log of a Completed pod. `dbctl plan` prints the
// exact statements the controller would run, against the real database, and
// changes nothing.
package main

import (
	"fmt"
	"os"

	"github.com/hashicorp/cli"
)

var version = "dev"

func main() {
	c := cli.NewCLI("dbctl", version)
	c.Args = os.Args[1:]
	c.Commands = map[string]cli.CommandFactory{
		"plan":  func() (cli.Command, error) { return &planCommand{}, nil },
		"apply": func() (cli.Command, error) { return &applyCommand{}, nil },
	}

	status, err := c.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dbctl: %v\n", err)
	}
	os.Exit(status)
}
