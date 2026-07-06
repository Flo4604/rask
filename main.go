// Package main provides rask's executable entrypoint.
package main

import (
	"os"

	"github.com/chronark/rask/cmd/clean"
	testcmd "github.com/chronark/rask/cmd/test"
	"github.com/chronark/rask/internal/cli"
	"github.com/chronark/rask/internal/setup"
)

// main assembles the root command and exits with its code.
func main() {
	testCommand := testcmd.Command()
	app := &cli.Command{
		Name:               "rask",
		Usage:              "Run Go tests with content-addressed result caching",
		Description:        "Testing takes package patterns plus -v/--verbose and --no-cache.",
		Flags:              testCommand.Flags,
		Action:             testCommand.Action,
		AcceptsArgs:        true,
		DefaultActionNames: []string{"test", "t"},
		UnknownFlagHint:    "rask supports only -v/--verbose and --no-cache; use `go test` for other flags",
		Commands: []*cli.Command{
			clean.Command(),
		},
	}
	os.Exit(setup.Execute(app, os.Args))
}
