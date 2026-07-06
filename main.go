// Package main provides rask's executable entrypoint.
package main

import (
	"os"

	"github.com/chronark/rask/internal/cmd"
)

// main passes process arguments to [cmd.Execute] and exits with its code.
func main() {
	os.Exit(cmd.Execute(os.Args))
}
