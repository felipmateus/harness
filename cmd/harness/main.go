// Command harness is the harness CLI and daemon. All of it lives in internal/cli;
// cmd/harness may depend on that package alone.
package main

import (
	"os"

	"github.com/felipmateus/harness/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
