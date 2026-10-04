// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

// Command dbctl runs the controller's plan from a terminal. `dbctl plan` prints
// the statements the controller would run against the real database and
// changes nothing; `dbctl apply` runs them.
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
